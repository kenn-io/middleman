package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/ptyowner"
	"go.kenn.io/forge/internal/ptysize"
	"go.kenn.io/forge/internal/testutil/dbtest"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	return dbtest.Open(t)
}

// staticBaseResolver returns a resolver that always reports path as the
// configured local worktree base.
func staticBaseResolver(path string) WorktreeBasePathResolver {
	return func(
		context.Context, WorktreeBaseRepository,
	) (string, bool, error) {
		return path, true, nil
	}
}

func seedRepo(
	t *testing.T, d *db.DB,
	host, owner, name string,
) int64 {
	t.Helper()
	identity := db.GitHubRepoIdentity(host, owner, name)
	identity.PlatformRepoID = "repo-" + owner + "-" + name
	id, err := d.UpsertRepo(
		t.Context(), identity,
	)
	require.NoError(t, err)
	return id
}

func seedMR(
	t *testing.T, d *db.DB,
	repoID int64, number int, headBranch string,
) {
	t.Helper()
	repo, err := d.GetRepoByID(t.Context(), repoID)
	require.NoError(t, err)
	require.NotNil(t, repo)
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mr := &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       repoID*10000 + int64(number),
		Number:           number,
		Title:            "Test PR",
		Author:           "author",
		State:            "open",
		HeadBranch:       headBranch,
		HeadRepoCloneURL: "https://" + repo.PlatformHost + "/" + repo.Owner + "/" + repo.Name + ".git",
		BaseBranch:       "main",
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}
	_, err = d.UpsertMergeRequest(t.Context(), mr)
	require.NoError(t, err)
}

func seedMRWithHeadRepo(
	t *testing.T, d *db.DB,
	repoID int64, number int,
	headBranch, cloneURL string,
) {
	t.Helper()
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mr := &db.MergeRequest{
		RepoID:           repoID,
		PlatformID:       repoID*10000 + int64(number),
		Number:           number,
		Title:            "PR with head repo",
		Author:           "contributor",
		State:            "open",
		HeadBranch:       headBranch,
		BaseBranch:       "main",
		HeadRepoCloneURL: cloneURL,
		CreatedAt:        now,
		UpdatedAt:        now,
		LastActivityAt:   now,
	}
	_, err := d.UpsertMergeRequest(t.Context(), mr)
	require.NoError(t, err)
}

func pullLaunchSpecForWorkspace(
	ws *Workspace, headRepoKind, headRepoCloneURL string,
) *WorkspaceLaunchSpec {
	issuedAt := time.Now().UTC()
	return &WorkspaceLaunchSpec{
		Version: WorkspaceLaunchSpecVersion,
		Repository: WorkspaceLaunchRepository{
			Provider: ws.Platform, PlatformHost: ws.PlatformHost,
			PlatformRepoID: "repo-test", Owner: firstNonEmpty(ws.RepoOwner, "acme"),
			Name: firstNonEmpty(ws.RepoName, "widget"),
			CloneURL: "https://" + ws.PlatformHost + "/" +
				firstNonEmpty(ws.RepoOwner, "acme") + "/" +
				firstNonEmpty(ws.RepoName, "widget") + ".git",
			DefaultBranch: "main",
		},
		ItemType: ws.ItemType, ItemNumber: ws.ItemNumber,
		ItemKey: strconv.Itoa(ws.ItemNumber), GitHeadRef: ws.GitHeadRef,
		Pull: &WorkspaceLaunchPull{
			HeadBranch: ws.GitHeadRef, HeadRepoKind: headRepoKind,
			HeadRepoCloneURL: headRepoCloneURL, SnapshotRevision: 1,
		},
		SourceVisible: true, IssuedAt: issuedAt,
		SourceVisibleUntil: issuedAt.Add(WorkspaceLaunchSpecVisibilityLease),
	}
}

func recordRuntimeTmuxSessionForTest(
	t *testing.T,
	d *db.DB,
	workspaceID string,
	sessionKey string,
	targetKey string,
	tmuxSession string,
	createdAt time.Time,
) {
	t.Helper()
	require.NoError(t, d.UpsertWorkspaceRuntimeSession(
		t.Context(),
		&db.WorkspaceRuntimeSession{
			WorkspaceID: workspaceID,
			SessionKey:  sessionKey,
			TargetKey:   targetKey,
			Label:       targetKey,
			Kind:        "agent",
			Scope:       "session",
			TmuxSession: tmuxSession,
			CreatedAt:   createdAt,
		},
	))
}

func TestCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	wtDir := t.TempDir()

	repoID := seedRepo(
		t, d, "github.com", "acme", "widget",
	)
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)

	ws, err := mgr.Create(
		ctx, "github", "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(ws)

	assert.NotEmpty(ws.ID)
	assert.Len(ws.ID, 16) // 8 bytes hex-encoded
	assert.Equal("creating", ws.Status)
	assert.Equal("github.com", ws.PlatformHost)
	assert.Equal("acme", ws.RepoOwner)
	assert.Equal("widget", ws.RepoName)
	assert.Equal(db.WorkspaceItemTypePullRequest, ws.ItemType)
	assert.Equal(42, ws.ItemNumber)
	assert.Equal("feature/thing", ws.GitHeadRef)
	assert.Nil(ws.MRHeadRepo)
	assert.Contains(ws.WorktreePath, "pr-42")
	assert.Equal("forge-"+ws.ID, ws.TmuxSession)

	// Verify persisted in DB.
	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(ws.ID, got.ID)
	assert.Equal("creating", got.Status)
}

func TestListSummariesUsesCacheWhenStoreHasNoRows(t *testing.T) {
	t.Parallel()

	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())

	mgr.setWorkspaceSummaryCache([]WorkspaceSummary{{
		ID:           "cached-workspace",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature/cache-workspace",
		Status:       "ready",
		CreatedAt:    time.Now().UTC(),
	}})
	got, err := mgr.ListSummaries(ctx)
	require.NoError(err)
	require.Len(got, 1)
	assert.Equal("cached-workspace", got[0].ID)
}

func TestWorkspaceSummaryCacheDoesNotResurrectDeletedWorkspace(t *testing.T) {
	t.Parallel()

	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 7, "feature/cache-workspace")
	mgr := newTestManager(t, d, t.TempDir())

	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 7)
	require.NoError(err)
	first, err := mgr.ListSummaries(ctx)
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal(ws.ID, first[0].ID)

	mgr.removeWorkspaceSummaryFromCache(ws.ID)
	mgr.setWorkspaceSummaryCache(first)
	assert.Empty(mgr.cachedWorkspaceSummaries())
}

func TestCreatePRHeadRepoClassification(t *testing.T) {
	tests := []struct {
		name           string
		provider       string
		platformHost   string
		owner          string
		repoName       string
		number         int
		headBranch     string
		headRepoURL    string
		wantMRHeadRepo string
		wantUnknown    bool
	}{
		{
			name:           "fork PR keeps head repo",
			number:         99,
			headBranch:     "fix/typo",
			headRepoURL:    "https://github.com/contributor/widget.git",
			wantMRHeadRepo: "https://github.com/contributor/widget.git",
		},
		{
			name:        "same-repo PR with populated head repo is not fork",
			number:      244,
			headBranch:  "feature/thing",
			headRepoURL: "git@GitHub.com:Acme/Widget.git",
		},
		{
			name:         "same-repo PR on enterprise host with port is not fork",
			platformHost: "ghe.example.com:8443",
			number:       246,
			headBranch:   "feature/enterprise",
			headRepoURL:  "https://GHE.example.com:8443/Acme/Widget.git",
		},
		{
			name:        "missing head repo metadata is unknown",
			number:      247,
			headBranch:  "feature/unknown",
			wantUnknown: true,
		},
		{
			name:         "same-repo GitLab nested group is not fork",
			provider:     "gitlab",
			platformHost: "gitlab.com",
			owner:        "group/subgroup",
			repoName:     "project",
			number:       248,
			headBranch:   "feature/nested",
			headRepoURL:  "https://gitlab.com/group/subgroup/project.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			d := openTestDB(t)
			provider := tt.provider
			if provider == "" {
				provider = "github"
			}
			platformHost := tt.platformHost
			if platformHost == "" {
				platformHost = "github.com"
			}
			owner := tt.owner
			if owner == "" {
				owner = "acme"
			}
			repoName := tt.repoName
			if repoName == "" {
				repoName = "widget"
			}
			repoID, err := d.UpsertRepo(t.Context(), db.RepoIdentity{
				Platform:       provider,
				PlatformHost:   platformHost,
				PlatformRepoID: "repo-" + provider + "-" + owner + "-" + repoName,
				Owner:          owner,
				Name:           repoName,
				RepoPath:       owner + "/" + repoName,
			})
			require.NoError(err)
			seedMRWithHeadRepo(
				t, d, repoID, tt.number, tt.headBranch, tt.headRepoURL,
			)

			mgr := newTestManager(t, d, t.TempDir())
			ws, err := mgr.Create(
				t.Context(), provider, platformHost, owner, repoName, tt.number,
			)
			require.NoError(err)
			require.NotNil(ws)

			if tt.wantUnknown {
				require.NotNil(ws.MRHeadRepo)
				assert.Empty(*ws.MRHeadRepo)
				return
			}
			if tt.wantMRHeadRepo == "" {
				// Same-repo PRs still have head repo clone URLs in GitHub
				// payloads. Keeping MRHeadRepo nil sends workspace setup down
				// the origin/<branch> path instead of the refs/pull/<number>/head
				// path reserved for forks.
				assert.Nil(ws.MRHeadRepo)
				return
			}
			require.NotNil(ws.MRHeadRepo)
			assert.Equal(tt.wantMRHeadRepo, *ws.MRHeadRepo)
		})
	}
}

func TestRefreshWorkspaceHeadRepo(t *testing.T) {
	tests := []struct {
		name        string
		cloneURL    string
		wantUnknown bool
		wantFork    string
	}{
		{
			name:     "current same-repo metadata",
			cloneURL: "https://github.com/acme/widget.git",
		},
		{
			name:        "missing current metadata",
			wantUnknown: true,
		},
		{
			name:     "current fork metadata",
			cloneURL: "https://github.com/contributor/widget.git",
			wantFork: "https://github.com/contributor/widget.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			d := openTestDB(t)
			repoID := seedRepo(t, d, "github.com", "acme", "widget")
			seedMRWithHeadRepo(t, d, repoID, 42, "feature/thing", tt.cloneURL)
			ws := &Workspace{
				Platform:     "github",
				PlatformHost: "github.com",
				RepoOwner:    "acme",
				RepoName:     "widget",
				ItemType:     db.WorkspaceItemTypePullRequest,
				ItemNumber:   42,
				GitHeadRef:   "feature/thing",
			}
			mgr := NewManager(d, t.TempDir())

			err := mgr.RefreshWorkspaceHeadRepo(t.Context(), ws)

			require.NoError(err)
			switch {
			case tt.wantUnknown:
				require.NotNil(ws.MRHeadRepo)
				assert.Empty(*ws.MRHeadRepo)
			case tt.wantFork != "":
				require.NotNil(ws.MRHeadRepo)
				assert.Equal(tt.wantFork, *ws.MRHeadRepo)
			default:
				assert.Nil(ws.MRHeadRepo)
			}
		})
	}
}

func TestRefreshWorkspaceHeadRepoRejectsPersistedWorkspaceWithoutRepositoryID(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ws := &Workspace{
		ID:           "ws-missing-repo",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "missing",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))
	mgr := NewManager(d, t.TempDir())

	err := mgr.RefreshWorkspaceHeadRepo(t.Context(), ws)

	require.ErrorIs(err, ErrWorkspaceRepositoryUnresolved)
	require.Nil(ws.MRHeadRepo)
	stored, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.Nil(stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoFollowsStableRepositoryAfterRename(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	providerRepoID := "gid://gitlab/Project/42"
	repoID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: providerRepoID,
		Owner:          "old-group",
		Name:           "old-name",
	})
	require.NoError(err)
	oldSameRepoURL := "https://gitlab.com/old-group/old-name.git"
	seedMRWithHeadRepo(t, d, repoID, 42, "feature/thing", oldSameRepoURL)
	ws := &Workspace{
		ID:           "ws-renamed-repo",
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		RepoOwner:    "old-group",
		RepoName:     "old-name",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: providerRepoID,
		Owner:          "new-group",
		Name:           "new-name",
	}, time.Now().UTC())
	require.NoError(err)

	mgr := NewManager(d, t.TempDir())
	require.NoError(mgr.RefreshWorkspaceHeadRepo(ctx, ws))

	assert.Equal("new-group", ws.RepoOwner)
	assert.Equal("new-name", ws.RepoName)
	require.NotNil(ws.MRHeadRepo)
	assert.Equal(oldSameRepoURL, *ws.MRHeadRepo)
	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("new-group", stored.RepoOwner)
	assert.Equal("new-name", stored.RepoName)
	require.NotNil(stored.MRHeadRepo)
	assert.Equal(oldSameRepoURL, *stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoRejectsReplacementAtInactiveRepositoryRoute(
	t *testing.T,
) {
	require := require.New(t)
	database := openTestDB(t)
	observedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	original, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-original", Owner: "acme", Name: "widget",
		}, observedAt,
	)
	require.NoError(err)
	require.NotNil(original)
	seedMRWithHeadRepo(
		t, database, original.Repository.ID, 42, "feature/original",
		"https://github.com/acme/widget.git",
	)
	workspace := &Workspace{
		ID: "ws-inactive-repository", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
		GitHeadRef: "feature/original", WorktreePath: t.TempDir(), Status: "ready",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.Equal(original.Repository.ID, workspace.RepoID)
	_, err = database.DeactivateRepositoryObservation(
		t.Context(), "github", "github.com", "repo-original",
		observedAt.Add(time.Minute),
	)
	require.NoError(err)
	replacement, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-replacement", Owner: "acme", Name: "widget",
		}, observedAt.Add(2*time.Minute),
	)
	require.NoError(err)
	require.NotNil(replacement)
	seedMRWithHeadRepo(
		t, database, replacement.Repository.ID, 42, "feature/replacement",
		"https://github.com/contributor/widget.git",
	)

	manager := NewManager(database, t.TempDir())
	err = manager.RefreshWorkspaceHeadRepo(t.Context(), workspace)

	require.ErrorIs(err, ErrWorkspaceRepositoryUnresolved)
	stored, err := database.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.Nil(stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoBlocksRepositoryReconciliation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	providerRepoID := "gid://gitlab/Project/42"
	sourceID, err := d.UpsertRepoByProviderID(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: providerRepoID,
		Owner:          "old-group",
		Name:           "old-name",
	})
	require.NoError(err)
	forkURL := "https://gitlab.com/contributor/widget.git"
	seedMRWithHeadRepo(t, d, sourceID, 42, "feature/thing", forkURL)
	_, err = d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "new-group",
		Name:         "new-name",
	})
	require.NoError(err)
	ws := &Workspace{
		ID:           "ws-reconciliation-barrier",
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		RepoOwner:    "old-group",
		RepoName:     "old-name",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	snapshotRead := make(chan struct{})
	continueRefresh := make(chan struct{})
	var signalSnapshot sync.Once
	mgr := NewManager(d, t.TempDir())
	mgr.afterHeadRepoSnapshotRead = func() {
		signalSnapshot.Do(func() { close(snapshotRead) })
		<-continueRefresh
	}
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- mgr.RefreshWorkspaceHeadRepo(ctx, ws)
	}()
	select {
	case <-snapshotRead:
	case <-time.After(5 * time.Second):
		require.Fail("head-repository refresh did not reach snapshot read")
	}

	writeLockAttempted := make(chan struct{})
	restoreWriteLockHook := d.SetBeforeRepositoryReconciliationWriteLockForTest(
		func() { close(writeLockAttempted) },
	)
	defer restoreWriteLockHook()
	reconciliationDone := make(chan error, 1)
	go func() {
		_, _, reconcileErr := d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
			Platform:       "gitlab",
			PlatformHost:   "gitlab.com",
			PlatformRepoID: providerRepoID,
			Owner:          "new-group",
			Name:           "new-name",
		}, time.Now().UTC())
		reconciliationDone <- reconcileErr
	}()
	select {
	case <-writeLockAttempted:
	case <-time.After(5 * time.Second):
		require.Fail("repository reconciliation did not attempt its write lock")
	}
	select {
	case upsertErr := <-reconciliationDone:
		require.NoError(upsertErr)
		require.Fail("repository reconciliation bypassed the active refresh barrier")
	default:
	}

	close(continueRefresh)
	select {
	case refreshErr := <-refreshDone:
		require.NoError(refreshErr)
	case <-time.After(5 * time.Second):
		require.Fail("head-repository refresh did not finish")
	}
	select {
	case upsertErr := <-reconciliationDone:
		require.NoError(upsertErr)
	case <-time.After(5 * time.Second):
		require.Fail("repository reconciliation did not resume")
	}

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("new-group", stored.RepoOwner)
	assert.Equal("new-name", stored.RepoName)
	require.NotNil(stored.MRHeadRepo)
	assert.Equal(forkURL, *stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoRejectsLegacyWorkspaceWhenRouteAppearsLater(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ws := &Workspace{
		ID:           "ws-repo-appears",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))
	mgr := NewManager(d, t.TempDir())
	forkURL := "https://github.com/contributor/widget.git"

	require.ErrorIs(
		mgr.RefreshWorkspaceHeadRepo(t.Context(), ws),
		ErrWorkspaceRepositoryUnresolved,
	)

	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMRWithHeadRepo(
		t, d, repoID, ws.ItemNumber, ws.GitHeadRef, forkURL,
	)

	err := mgr.RefreshWorkspaceHeadRepo(t.Context(), ws)

	require.ErrorIs(err, ErrWorkspaceRepositoryUnresolved)
	require.Nil(ws.MRHeadRepo)
	stored, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.Nil(stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoSnapshotRetriesAfterRevisionChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMRWithHeadRepo(
		t,
		d,
		repoID,
		42,
		"feature/thing",
		"https://github.com/acme/widget.git",
	)
	ws := &Workspace{
		ID:           "ws-refresh-head-repo-race",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))

	mgr := NewManager(d, t.TempDir())
	var advanced bool
	mgr.afterHeadRepoSnapshotRead = func() {
		if advanced {
			return
		}
		advanced = true
		current, err := d.GetMergeRequestByRepoIDAndNumber(
			t.Context(), repoID, ws.ItemNumber,
		)
		require.NoError(err)
		require.NotNil(current)
		updated := *current
		updated.HeadRepoCloneURL = "https://github.com/forker/widget.git"
		updated.UpdatedAt = updated.UpdatedAt.Add(time.Minute)
		updated.LastActivityAt = updated.UpdatedAt
		_, accepted, err := d.UpsertMergeRequestSnapshot(t.Context(), &updated)
		require.NoError(err)
		require.True(accepted)
	}

	snapshot, err := mgr.RefreshWorkspaceHeadRepoSnapshot(t.Context(), ws)

	require.NoError(err)
	require.NotNil(snapshot)
	require.NotNil(snapshot.MRHeadRepo)
	assert.Equal("https://github.com/forker/widget.git", *snapshot.MRHeadRepo)
	latest, err := d.GetMergeRequestByRepoIDAndNumber(
		t.Context(), repoID, ws.ItemNumber,
	)
	require.NoError(err)
	require.NotNil(latest)
	assert.Equal(latest.SnapshotRevision, snapshot.SnapshotRevision)
	stored, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.NotNil(stored.MRHeadRepo)
	assert.Equal("https://github.com/forker/widget.git", *stored.MRHeadRepo)
}

func TestRefreshWorkspaceHeadRepoSnapshotRetriesAfterRemoval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMRWithHeadRepo(
		t,
		d,
		repoID,
		42,
		"feature/thing",
		"https://github.com/acme/widget.git",
	)
	ws := &Workspace{
		ID:           "ws-refresh-head-repo-removal-race",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	mgr := NewManager(d, t.TempDir())
	var removed bool
	mgr.afterHeadRepoSnapshotRead = func() {
		if removed {
			return
		}
		removed = true
		now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
		_, err := d.WriteDB().ExecContext(ctx, `
			INSERT INTO forge_archive_items (
				repo_id, item_type, item_number, provider_item_id,
				provider_created_at, provider_updated_at, lifecycle_state
			) VALUES (?, 'merge_request', 42, 'pull-42', ?, ?, 'removed_upstream')`,
			repoID, now, now,
		)
		require.NoError(err)
	}

	snapshot, err := mgr.RefreshWorkspaceHeadRepoSnapshot(ctx, ws)

	require.NoError(err)
	require.NotNil(snapshot)
	require.NotNil(snapshot.MRHeadRepo)
	assert.Empty(*snapshot.MRHeadRepo)
	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	require.NotNil(stored.MRHeadRepo)
	assert.Empty(*stored.MRHeadRepo)
}

func TestCreateIssueDefaultBranchSluggified(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		slugStyle bool
		want      string
	}{
		{
			name:      "slug style with usable title",
			title:     "Add foo to bar",
			slugStyle: true,
			want:      "kenn-forge/issue-7-add-foo-to-bar",
		},
		{
			name:      "slug style with empty title falls back to bare",
			title:     "",
			slugStyle: true,
			want:      "kenn-forge/issue-7",
		},
		{
			name:      "slug style with all-punctuation falls back to bare",
			title:     "?!@#",
			slugStyle: true,
			want:      "kenn-forge/issue-7",
		},
		{
			name:      "bare style ignores title",
			title:     "Add foo to bar",
			slugStyle: false,
			want:      "kenn-forge/issue-7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			d := openTestDB(t)
			ctx := t.Context()
			repoID := seedRepo(t, d, "github.com", "acme", "widget")
			seedIssue(t, d, repoID, 7, tt.title)

			mgr := newTestManager(t, d, t.TempDir())
			mgr.SetIssueBranchSlugEnabled(tt.slugStyle)

			ws, err := mgr.CreateIssue(
				ctx, "github.com", "acme", "widget", 7,
				CreateIssueOptions{Provider: "github"},
			)
			require.NoError(err)
			require.NotNil(ws)

			assert.Equal(tt.want, ws.GitHeadRef)
			assert.Equal(tt.want, ws.WorkspaceBranch)
		})
	}
}

func TestCreateIssueExplicitGitHeadRefBypassesSlug(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := t.Context()
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedIssue(t, d, repoID, 7, "Add foo to bar")

	mgr := newTestManager(t, d, t.TempDir())

	ws, err := mgr.CreateIssue(
		ctx, "github.com", "acme", "widget", 7,
		CreateIssueOptions{Provider: "github", GitHeadRef: "custom/branch"},
	)
	require.NoError(err)
	require.NotNil(ws)
	assert.Equal("custom/branch", ws.GitHeadRef)
}

func TestCreateKataTaskDoesNotRequireProviderIssue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := t.Context()
	seedRepo(t, d, "github.com", "acme", "widget")

	mgr := newTestManager(t, d, t.TempDir())
	metadata := db.WorkspaceKataMetadata{
		DaemonID:    "desktop",
		ProjectUID:  "project-kata",
		ProjectName: "Kata",
		IssueUID:    "issue-kata-1",
		ShortID:     "task-123",
		QualifiedID: "Kata#task-123",
		Title:       "Fix widget",
	}
	ws, err := mgr.CreateKataTask(ctx, "github", "github.com", "acme", "widget", metadata)
	require.NoError(err)
	require.NotNil(ws)

	assert.Equal(db.WorkspaceItemTypeKataTask, ws.ItemType)
	assert.Equal(0, ws.ItemNumber)
	assert.Equal(db.KataWorkspaceItemKey(metadata), ws.ItemKey)
	assert.Contains(ws.GitHeadRef, "kenn-forge/kata/task-123-")
	assert.Contains(ws.GitHeadRef, "-fix-widget")
	assert.Equal(ws.GitHeadRef, ws.WorkspaceBranch)
	assert.Contains(ws.WorktreePath, "kata-task-123-")
	require.NotNil(ws.KataMetadata)
	assert.Equal("issue-kata-1", ws.KataMetadata.IssueUID)

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(db.WorkspaceItemTypeKataTask, got.ItemType)
	assert.Equal(db.KataWorkspaceItemKey(metadata), got.ItemKey)
	require.NotNil(got.KataMetadata)
	assert.Equal("Fix widget", got.KataMetadata.Title)
}

func TestCreateKataTaskNormalizesRelativeWorktreeDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cwd := filepath.Join(t.TempDir(), "cwd")
	require.NoError(os.MkdirAll(cwd, 0o755))
	t.Chdir(cwd)

	d := openTestDB(t)
	ctx := t.Context()
	repoID := seedRepo(t, d, "github.com", "acme", "widget")

	mgr := newTestManager(t, d, "relative-worktrees")
	metadata := db.WorkspaceKataMetadata{
		DaemonID:   "desktop",
		ProjectUID: "project-kata",
		IssueUID:   "issue-kata-1",
		ShortID:    "task-123",
		Title:      "Fix widget",
	}
	ws, err := mgr.CreateKataTask(ctx, "github", "github.com", "acme", "widget", metadata)
	require.NoError(err)
	require.NotNil(ws)

	assert.True(filepath.IsAbs(ws.WorktreePath))
	assert.Equal(
		filepath.Join(
			cwd, "relative-worktrees", "github", "github.com",
			"acme", "widget", fmt.Sprintf("repo-%d", repoID),
			"kata-"+kataTaskBranchID(metadata),
		),
		ws.WorktreePath,
	)
}

func TestCreateKataTaskScopesItemKeyByDaemonAndProject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := t.Context()
	seedRepo(t, d, "github.com", "acme", "widget")

	mgr := newTestManager(t, d, t.TempDir())
	first := db.WorkspaceKataMetadata{
		DaemonID:   "desktop",
		ProjectUID: "project-kata",
		IssueUID:   "shared-issue",
		ShortID:    "task-1",
		Title:      "Fix desktop task",
	}
	second := db.WorkspaceKataMetadata{
		DaemonID:   "laptop",
		ProjectUID: "project-kata",
		IssueUID:   "shared-issue",
		ShortID:    "task-1",
		Title:      "Fix laptop task",
	}

	ws1, err := mgr.CreateKataTask(ctx, "github", "github.com", "acme", "widget", first)
	require.NoError(err)
	ws2, err := mgr.CreateKataTask(ctx, "github", "github.com", "acme", "widget", second)
	require.NoError(err)

	assert.NotEqual(ws1.ItemKey, ws2.ItemKey)
	assert.Equal(db.KataWorkspaceItemKey(first), ws1.ItemKey)
	assert.Equal(db.KataWorkspaceItemKey(second), ws2.ItemKey)
	assert.NotEqual(ws1.GitHeadRef, ws2.GitHeadRef)
	assert.NotEqual(ws1.WorktreePath, ws2.WorktreePath)
	assert.Contains(ws1.GitHeadRef, "kenn-forge/kata/task-1-")
	assert.Contains(ws2.GitHeadRef, "kenn-forge/kata/task-1-")
}

func TestCreateIssueReuseLocalBaseBranchCheckedOutReturnsConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	ctx := t.Context()
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	const branch = "kenn-forge/issue-7"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", filepath.Join(t.TempDir(), "existing"),
		"-b", branch, "HEAD",
	)

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateIssue(
		ctx, "github.com", "acme", "widget", 7,
		CreateIssueOptions{Provider: "github", ReuseExistingBranch: true},
	)

	require.Nil(ws)
	var conflict *WorkspaceBranchConflictError
	require.ErrorAs(err, &conflict)
	require.NotNil(conflict)
	assert.Equal(branch, conflict.Branch)
	assert.Equal(branch+"-2", conflict.SuggestedBranch)
}

func TestCreateIssueRecoversExpectedExistingDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/base",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	const branch = "kenn-forge/issue-7"
	expectedPath := filepath.Join(
		worktreeRoot, "github", platformHost, "acme", "widget",
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", expectedPath, "-b", branch, "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "base.txt"), []byte("dirty\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "untracked.txt"), []byte("keep\n"), 0o644,
	))
	wantHead := strings.TrimSpace(string(runWorkspaceTestGit(
		t, expectedPath, "rev-parse", "HEAD",
	)))

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{
			Provider:               "github",
			GitHeadRef:             branch,
			ReuseExistingDirectory: true,
		},
	)

	require.NoError(err)
	require.NotNil(ws)
	assert.Equal(expectedPath, ws.WorktreePath)
	assert.Equal(workspaceBranchRecoveryPending, ws.WorkspaceBranch)
	assert.Equal(wantHead, strings.TrimSpace(string(runWorkspaceTestGit(
		t, expectedPath, "rev-parse", "HEAD",
	))))
	assert.FileExists(filepath.Join(expectedPath, "untracked.txt"))
	status := strings.TrimSpace(string(runWorkspaceTestGit(
		t, expectedPath, "status", "--short",
	)))
	assert.Contains(status, "M base.txt")
	assert.Contains(status, "?? untracked.txt")
}

func TestIssueWorkspaceBranchDoesNotCollideWithRecoveryState(t *testing.T) {
	require.Error(t, validateLocalBranchName(
		t.Context(), "", workspaceBranchRecoveryPending,
	))

	for _, recoverExisting := range []bool{false, true} {
		name := "new workspace"
		if recoverExisting {
			name = "recovered workspace"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			d := openTestDB(t)
			worktreeRoot := t.TempDir()
			localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
				t, "feature/base",
			)
			repoID := seedRepo(t, d, platformHost, "acme", "widget")
			seedIssue(t, d, repoID, 7, "")

			const branch = "__kenn_forge_recovery_pending__"
			expectedPath := filepath.Join(
				worktreeRoot, "github", platformHost, "acme", "widget",
				fmt.Sprintf("repo-%d", repoID), "issue-7",
			)
			if recoverExisting {
				runWorkspaceTestGit(
					t, localRepo,
					"worktree", "add", expectedPath, "-b", branch, "HEAD",
				)
			}

			mgr := newTestManager(t, d, worktreeRoot)
			mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
			tmuxScript, _ := writeRecorderScript(t)
			mgr.SetTmuxCommand([]string{tmuxScript})
			ws, err := mgr.CreateIssue(
				t.Context(), platformHost, "acme", "widget", 7,
				CreateIssueOptions{
					Provider:               "github",
					GitHeadRef:             branch,
					ReuseExistingDirectory: recoverExisting,
				},
			)
			require.NoError(err)
			require.NoError(mgr.Setup(t.Context(), ws))
			assert.Equal(branch, ws.WorkspaceBranch)
			assert.Equal("ready", ws.Status)
			assert.False(workspaceRequiresExistingDirectory(ws))

			dirty, err := mgr.Delete(t.Context(), ws.ID, true, nil)
			require.NoError(err)
			assert.Empty(dirty)
			assert.NoDirExists(expectedPath)
		})
	}
}

func TestCreateIssueRecoveryRejectsInvalidExpectedDirectory(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(t *testing.T, localRepo, expectedPath, branch string)
		wantReason WorkspaceDirectoryRecoveryReason
	}{
		{
			name:       "missing",
			prepare:    func(*testing.T, string, string, string) {},
			wantReason: WorkspaceDirectoryMissing,
		},
		{
			name: "ordinary directory",
			prepare: func(t *testing.T, _, expectedPath, _ string) {
				require.NoError(t, os.MkdirAll(expectedPath, 0o755))
			},
			wantReason: WorkspaceDirectoryNotLinkedWorktree,
		},
		{
			name: "wrong branch",
			prepare: func(t *testing.T, localRepo, expectedPath, _ string) {
				runWorkspaceTestGit(
					t, localRepo,
					"worktree", "add", expectedPath, "-b", "other/branch", "HEAD",
				)
			},
			wantReason: WorkspaceDirectoryBranchMismatch,
		},
		{
			name: "wrong repository",
			prepare: func(t *testing.T, _, expectedPath, branch string) {
				otherRepo := filepath.Join(t.TempDir(), "other")
				runWorkspaceTestGit(
					t, filepath.Dir(otherRepo),
					"init", "--initial-branch=main", otherRepo,
				)
				runWorkspaceTestGit(
					t, otherRepo, "config", "user.email", "test@test.com",
				)
				runWorkspaceTestGit(
					t, otherRepo, "config", "user.name", "Test",
				)
				require.NoError(t, os.WriteFile(
					filepath.Join(otherRepo, "base.txt"), []byte("base\n"), 0o644,
				))
				runWorkspaceTestGit(t, otherRepo, "add", ".")
				runWorkspaceTestGit(t, otherRepo, "commit", "-m", "base")
				runWorkspaceTestGit(
					t, otherRepo,
					"worktree", "add", expectedPath, "-b", branch, "HEAD",
				)
			},
			wantReason: WorkspaceDirectoryRepositoryMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			d := openTestDB(t)
			worktreeRoot := t.TempDir()
			localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
				t, "feature/base",
			)
			repoID := seedRepo(t, d, platformHost, "acme", "widget")
			seedIssue(t, d, repoID, 7, "")
			const branch = "kenn-forge/issue-7"
			expectedPath := filepath.Join(
				worktreeRoot, "github", platformHost, "acme", "widget",
				fmt.Sprintf("repo-%d", repoID), "issue-7",
			)
			tt.prepare(t, localRepo, expectedPath, branch)

			mgr := newTestManager(t, d, worktreeRoot)
			mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
			ws, err := mgr.CreateIssue(
				t.Context(), platformHost, "acme", "widget", 7,
				CreateIssueOptions{
					Provider:               "github",
					GitHeadRef:             branch,
					ReuseExistingDirectory: true,
				},
			)

			require.Nil(ws)
			var recoveryErr *WorkspaceDirectoryRecoveryError
			require.ErrorAs(err, &recoveryErr)
			require.NotNil(recoveryErr)
			assert.Equal(tt.wantReason, recoveryErr.Reason)
			stored, getErr := d.GetWorkspaceByIssueForProvider(
				t.Context(), "github", platformHost, "acme", "widget", 7,
			)
			require.NoError(getErr)
			assert.Nil(stored)
		})
	}
}

func TestCreateIssueRecoveryRejectsManagedCloneWithWrongOrigin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	const (
		host   = "github.com"
		owner  = "acme"
		name   = "widget"
		branch = "kenn-forge/issue-7"
	)
	repoID := seedRepo(t, d, host, owner, name)
	seedIssue(t, d, repoID, 7, "")

	clones := gitclone.New(t.TempDir(), nil)
	cloneDir, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", host, owner, name,
	)
	require.NoError(err)
	seedWorkspaceBareCloneAt(t, cloneDir)
	runWorkspaceTestGit(
		t, cloneDir, "remote", "set-url", "origin",
		"https://github.com/other/repository.git",
	)
	expectedPath := filepath.Join(
		worktreeRoot, "github", host, owner, name,
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, cloneDir,
		"worktree", "add", expectedPath, "-b", branch, "HEAD",
	)

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetClones(clones)
	ws, err := mgr.CreateIssue(
		t.Context(), host, owner, name, 7,
		CreateIssueOptions{
			Provider:               "github",
			GitHeadRef:             branch,
			ReuseExistingDirectory: true,
		},
	)

	require.Nil(ws)
	var recoveryErr *WorkspaceDirectoryRecoveryError
	require.ErrorAs(err, &recoveryErr)
	assert.Equal(WorkspaceDirectoryRepositoryMismatch, recoveryErr.Reason)
}

func TestSetupRecoveryRejectsManagedCloneWhoseOriginChanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	const (
		host   = "github.com"
		owner  = "acme"
		name   = "widget"
		branch = "kenn-forge/issue-7"
	)
	repoID := seedRepo(t, d, host, owner, name)
	seedIssue(t, d, repoID, 7, "")

	mgr := newTestManager(t, d, worktreeRoot)
	ws, err := mgr.CreateIssue(
		t.Context(), host, owner, name, 7,
		CreateIssueOptions{Provider: "github", GitHeadRef: branch},
	)
	require.NoError(err)
	require.NotNil(ws)

	clones := gitclone.New(t.TempDir(), nil)
	cloneDir, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", host, owner, name,
	)
	require.NoError(err)
	seedWorkspaceBareCloneAt(t, cloneDir)
	runWorkspaceTestGit(
		t, cloneDir, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(
		t, cloneDir,
		"worktree", "add", ws.WorktreePath, "-b", branch, "HEAD",
	)

	mgr.SetClones(clones)
	runWorkspaceTestGit(
		t, cloneDir, "remote", "set-url", "origin",
		"https://github.com/other/repository.git",
	)

	err = mgr.Setup(t.Context(), ws)

	var recoveryErr *WorkspaceDirectoryRecoveryError
	require.ErrorAs(err, &recoveryErr)
	assert.Equal(WorkspaceDirectoryRepositoryMismatch, recoveryErr.Reason)
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
}

func TestSetupReusesPreIdentityManagedCloneAfterRepositoryBackfill(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	worktreeRoot := t.TempDir()
	_, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/seven",
	)
	remoteURL := "http://" + platformHost + "/acme/widget.git"
	repoID := seedRepo(t, database, platformHost, "acme", "widget")
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			CloneURL: remoteURL, DefaultBranch: "main",
		},
	))

	spec := launchSpecForTest()
	spec.Repository.PlatformHost = platformHost
	spec.Repository.PlatformRepoID = "repo-acme-widget"
	spec.Repository.CloneURL = remoteURL
	manager := NewManager(database, worktreeRoot)
	manager.SetNow(func() time.Time { return spec.IssuedAt })
	workspace, err := manager.CreateFromLaunchSpec(t.Context(), spec)
	require.NoError(err)

	clones := gitclone.New(t.TempDir(), nil)
	require.NoError(clones.EnsureClone(
		t.Context(), "github", platformHost, "acme", "widget", remoteURL,
	))
	legacyClone, err := clones.ClonePath("github", platformHost, "acme", "widget")
	require.NoError(err)
	runWorkspaceTestGit(
		t, legacyClone, "worktree", "add", "-b", syntheticPRWorktreeBranch(spec.ItemNumber),
		workspace.WorktreePath, "refs/remotes/origin/"+spec.GitHeadRef,
	)
	manager.SetClones(clones)

	err = manager.Setup(t.Context(), workspace)

	require.NoError(err)
	persisted, err := database.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(err)
	require.NotNil(persisted)
	require.Equal("ready", persisted.Status)
	commonDir, err := worktreeCommonGitDir(t.Context(), workspace.WorktreePath)
	require.NoError(err)
	canonicalLegacyClone, err := canonicalFilesystemPath(legacyClone)
	require.NoError(err)
	require.Equal(canonicalLegacyClone, commonDir)
}

func TestSetupReusesIdentityManagedCloneAfterRepositoryRename(t *testing.T) {
	testSetupReusesManagedCloneAfterRepositoryRename(t, true)
}

func TestSetupReusesPreIdentityManagedCloneAfterRepositoryRename(t *testing.T) {
	testSetupReusesManagedCloneAfterRepositoryRename(t, false)
}

func testSetupReusesManagedCloneAfterRepositoryRename(
	t *testing.T, identityScoped bool,
) {
	require := require.New(t)
	database := openTestDB(t)
	worktreeRoot := t.TempDir()
	_, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/seven",
	)
	renamedRemote := filepath.Join(filepath.Dir(remote), "renamed.git")
	require.NoError(os.Symlink(remote, renamedRemote))
	twiceRenamedRemote := filepath.Join(filepath.Dir(remote), "twice-renamed.git")
	require.NoError(os.Symlink(remote, twiceRenamedRemote))
	oldRemoteURL := "http://" + platformHost + "/acme/widget.git"
	newRemoteURL := "http://" + platformHost + "/acme/renamed.git"
	twiceRenamedRemoteURL := "http://" + platformHost + "/acme/twice-renamed.git"
	repoID := seedRepo(t, database, platformHost, "acme", "widget")
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			CloneURL: oldRemoteURL, DefaultBranch: "main",
		},
	))

	spec := launchSpecForTest()
	spec.Repository.PlatformHost = platformHost
	spec.Repository.PlatformRepoID = "repo-acme-widget"
	spec.Repository.CloneURL = oldRemoteURL
	manager := NewManager(database, worktreeRoot)
	manager.SetTmuxCommand([]string{"/usr/bin/true"})
	manager.SetNow(func() time.Time { return spec.IssuedAt })
	workspace, err := manager.CreateFromLaunchSpec(t.Context(), spec)
	require.NoError(err)

	clones := gitclone.New(t.TempDir(), nil)
	cloneCtx := t.Context()
	if identityScoped {
		cloneCtx = gitclone.WithRepositoryIdentity(
			cloneCtx, spec.Repository.PlatformRepoID,
		)
	}
	require.NoError(clones.EnsureClone(
		cloneCtx, "github", platformHost, "acme", "widget", oldRemoteURL,
	))
	oldClone, err := clones.ClonePathForContext(
		cloneCtx, "github", platformHost, "acme", "widget",
	)
	require.NoError(err)
	runWorkspaceTestGit(
		t, oldClone, "worktree", "add", "-b", syntheticPRWorktreeBranch(spec.ItemNumber),
		workspace.WorktreePath, "refs/remotes/origin/"+spec.GitHeadRef,
	)
	manager.SetClones(clones)

	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: platformHost,
			PlatformRepoID: spec.Repository.PlatformRepoID,
			Owner:          "acme", Name: "renamed",
		}, time.Now().UTC().Add(time.Hour),
	)
	require.NoError(err)
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			CloneURL: newRemoteURL, DefaultBranch: "main",
		},
	))
	renamedSpec := spec
	renamedSpec.Repository.Name = "renamed"
	renamedSpec.Repository.CloneURL = newRemoteURL
	renamedSpec.IssuedAt = spec.IssuedAt.Add(time.Minute)
	renamedSpec.SourceVisibleUntil = renamedSpec.IssuedAt.Add(
		WorkspaceLaunchSpecVisibilityLease,
	)
	manager.SetLaunchSpecResolver(&staticLaunchSpecResolver{spec: renamedSpec})
	manager.SetNow(func() time.Time { return renamedSpec.IssuedAt })

	err = manager.Setup(t.Context(), workspace)

	require.NoError(err)
	runWorkspaceTestGit(t, oldClone, "remote", "set-url", "origin", oldRemoteURL)
	validationCalls := 0
	_, err = manager.reuseExistingWorkspaceWorktreeDetails(
		t.Context(), workspace, &renamedSpec,
		func(context.Context) error {
			validationCalls++
			if validationCalls == 3 {
				return db.ErrRepositoryRouteFenceChanged
			}
			return nil
		},
	)
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	originURLs, err := gitConfigValues(t.Context(), oldClone, "remote.origin.url")
	require.NoError(err)
	require.Equal([]string{oldRemoteURL}, originURLs,
		"a rejected refresh must restore the historical origin")
	require.NoError(manager.Setup(t.Context(), workspace),
		"a retargeted historical clone must remain reusable")
	persisted, err := database.GetWorkspace(t.Context(), workspace.ID)
	require.NoError(err)
	require.NotNil(persisted)
	require.Equal("ready", persisted.Status)
	commonDir, err := worktreeCommonGitDir(t.Context(), workspace.WorktreePath)
	require.NoError(err)
	canonicalOldClone, err := canonicalFilesystemPath(oldClone)
	require.NoError(err)
	require.Equal(canonicalOldClone, commonDir)
	originURLs, err = gitConfigValues(t.Context(), oldClone, "remote.origin.url")
	require.NoError(err)
	require.Equal([]string{newRemoteURL}, originURLs)

	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: platformHost,
			PlatformRepoID: spec.Repository.PlatformRepoID,
			Owner:          "acme", Name: "twice-renamed",
		}, time.Now().UTC().Add(2*time.Hour),
	)
	require.NoError(err)
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			CloneURL: twiceRenamedRemoteURL, DefaultBranch: "main",
		},
	))
	twiceRenamedSpec := renamedSpec
	twiceRenamedSpec.Repository.Name = "twice-renamed"
	twiceRenamedSpec.Repository.CloneURL = twiceRenamedRemoteURL
	twiceRenamedSpec.IssuedAt = renamedSpec.IssuedAt.Add(time.Minute)
	twiceRenamedSpec.SourceVisibleUntil = twiceRenamedSpec.IssuedAt.Add(
		WorkspaceLaunchSpecVisibilityLease,
	)
	manager.SetLaunchSpecResolver(&staticLaunchSpecResolver{spec: twiceRenamedSpec})
	manager.SetNow(func() time.Time { return twiceRenamedSpec.IssuedAt })

	require.NoError(manager.Setup(t.Context(), workspace),
		"a clone retargeted by an earlier rename must survive another rename")
	commonDir, err = worktreeCommonGitDir(t.Context(), workspace.WorktreePath)
	require.NoError(err)
	require.Equal(canonicalOldClone, commonDir)
	originURLs, err = gitConfigValues(t.Context(), oldClone, "remote.origin.url")
	require.NoError(err)
	require.Equal([]string{twiceRenamedRemoteURL}, originURLs)
}

func TestManagedClonePathsExcludeLegacyRouteAfterReuse(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	observedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	original := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-original", Owner: "acme", Name: "widget",
	}
	entry, _, err := database.ReconcileRepositoryObservation(
		t.Context(), original, observedAt,
	)
	require.NoError(err)
	require.NotNil(entry)

	original.Name = "widget-original"
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), original, observedAt.Add(time.Minute),
	)
	require.NoError(err)
	replacement := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-replacement", Owner: "acme", Name: "widget",
	}
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), replacement, observedAt.Add(2*time.Minute),
	)
	require.NoError(err)
	replacement.Name = "widget-replacement"
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), replacement, observedAt.Add(3*time.Minute),
	)
	require.NoError(err)
	original.Name = "widget"
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), original, observedAt.Add(4*time.Minute),
	)
	require.NoError(err)

	clones := gitclone.New(t.TempDir(), nil)
	manager := NewManager(database, t.TempDir())
	manager.SetClones(clones)
	paths, err := manager.workspaceManagedClonePaths(t.Context(), &Workspace{
		RepoID: entry.Repository.ID, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
	})

	require.NoError(err)
	require.Len(paths, 3)
	legacyPath, err := clones.ClonePath("github", "github.com", "acme", "widget")
	require.NoError(err)
	require.NotContains(paths, legacyPath)
	historicalPath, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), original.PlatformRepoID),
		"github", "github.com", "acme", "widget-original",
	)
	require.NoError(err)
	require.Contains(paths, historicalPath)
	historicalLegacyPath, err := clones.ClonePath(
		"github", "github.com", "acme", "widget-original",
	)
	require.NoError(err)
	require.Contains(paths, historicalLegacyPath)
}

func TestCreateIssueReportsRecoverableDirectoryBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/base",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "renamed issue title")

	const existingBranch = "kenn-forge/issue-7-original-title"
	expectedPath := filepath.Join(
		worktreeRoot, "github", platformHost, "acme", "widget",
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", expectedPath, "-b", existingBranch, "HEAD",
	)

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{Provider: "github"},
	)

	require.Nil(ws)
	var conflict *WorkspaceBranchConflictError
	require.ErrorAs(err, &conflict)
	require.NotNil(conflict)
	assert.Equal(existingBranch, conflict.Branch)
	stored, getErr := d.GetWorkspaceByIssueForProvider(
		t.Context(), "github", platformHost, "acme", "widget", 7,
	)
	require.NoError(getErr)
	assert.Nil(stored)
}

func TestSetupDirectoryRecoveryNeverCreatesReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/base",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	const branch = "kenn-forge/issue-7"
	expectedPath := filepath.Join(
		worktreeRoot, "github", platformHost, "acme", "widget",
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", expectedPath, "-b", branch, "HEAD",
	)

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	tmuxScript, _ := writeRecorderScript(t)
	mgr.SetTmuxCommand([]string{tmuxScript})
	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{
			Provider:               "github",
			GitHeadRef:             branch,
			ReuseExistingDirectory: true,
		},
	)
	require.NoError(err)
	require.NotNil(ws)
	runWorkspaceTestGit(
		t, localRepo, "worktree", "remove", "--force", expectedPath,
	)
	runWorkspaceTestGit(t, localRepo, "branch", "-D", branch)

	err = mgr.Setup(t.Context(), ws)

	var recoveryErr *WorkspaceDirectoryRecoveryError
	require.ErrorAs(err, &recoveryErr)
	require.NotNil(recoveryErr)
	assert.Equal(WorkspaceDirectoryMissing, recoveryErr.Reason)
	assert.NoDirExists(expectedPath)
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
}

func TestRetryDirectoryRecoveryPreservesExistingWorktree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/base",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	const branch = "kenn-forge/issue-7"
	expectedPath := filepath.Join(
		worktreeRoot, "github", platformHost, "acme", "widget",
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", expectedPath, "-b", branch, "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "untracked.txt"), []byte("keep\n"), 0o644,
	))

	dir := t.TempDir()
	tmuxScript := filepath.Join(dir, "fake-tmux")
	response := "#!/bin/sh\n" +
		`for arg in "$@"; do` + "\n" +
		`  if [ "$arg" = "new-session" ]; then exit 1; fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(tmuxScript, []byte(response), 0o755))

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	mgr.SetTmuxCommand([]string{tmuxScript})
	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{
			Provider:               "github",
			GitHeadRef:             branch,
			ReuseExistingDirectory: true,
		},
	)
	require.NoError(err)
	require.NotNil(ws)
	require.Error(mgr.Setup(t.Context(), ws))
	assert.FileExists(filepath.Join(expectedPath, "untracked.txt"))

	next, startNow, err := mgr.RequestRetry(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.True(startNow)
	assert.DirExists(expectedPath)
	assert.FileExists(filepath.Join(expectedPath, "untracked.txt"))
}

func TestDeletePendingDirectoryRecoveryPreservesExistingWorktree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	worktreeRoot := t.TempDir()
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/base",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	const branch = "kenn-forge/issue-7"
	expectedPath := filepath.Join(
		worktreeRoot, "github", platformHost, "acme", "widget",
		fmt.Sprintf("repo-%d", repoID), "issue-7",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", expectedPath, "-b", branch, "HEAD",
	)
	require.NoError(os.WriteFile(
		filepath.Join(expectedPath, "untracked.txt"), []byte("keep\n"), 0o644,
	))

	mgr := newTestManager(t, d, worktreeRoot)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	tmuxScript, _ := writeRecorderScript(t)
	mgr.SetTmuxCommand([]string{tmuxScript})
	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{
			Provider:               "github",
			GitHeadRef:             branch,
			ReuseExistingDirectory: true,
		},
	)
	require.NoError(err)
	require.NotNil(ws)

	dirty, err := mgr.Delete(t.Context(), ws.ID, true, nil)

	require.NoError(err)
	assert.Empty(dirty)
	assert.DirExists(expectedPath)
	assert.FileExists(filepath.Join(expectedPath, "untracked.txt"))
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	assert.Nil(stored)
}

func TestCreateRepoNotTracked(t *testing.T) {
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())

	_, err := mgr.Create(
		t.Context(), "github", "github.com", "unknown", "repo", 1,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

func TestCreateDuplicate(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	wtDir := t.TempDir()

	repoID := seedRepo(
		t, d, "github.com", "acme", "widget",
	)
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)

	// First create succeeds.
	ws, err := mgr.Create(
		ctx, "github", "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(ws)

	// Second create for same MR fails with unique constraint.
	_, err = mgr.Create(
		ctx, "github", "github.com", "acme", "widget", 42,
	)
	require.Error(err)
	require.ErrorIs(err, ErrWorkspaceDuplicate)
}

func TestCreateMRNotSynced(t *testing.T) {
	d := openTestDB(t)

	seedRepo(t, d, "github.com", "acme", "widget")

	mgr := newTestManager(t, d, t.TempDir())

	_, err := mgr.Create(
		t.Context(), "github", "github.com", "acme", "widget", 999,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrWorkspaceNotSynced)
}

func TestSetupFailurePersistsStatusWhenContextCanceled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	repoID := seedRepo(
		t, d, "github.com", "acme", "widget",
	)
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)
	ws, err := mgr.Create(
		t.Context(), "github", "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(ws)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = mgr.Setup(ctx, ws)
	require.Error(err)
	require.ErrorIs(err, context.Canceled)

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Contains(*got.ErrorMessage, "context canceled")

	events, err := d.ListWorkspaceSetupEvents(
		t.Context(), ws.ID,
	)
	require.NoError(err)
	require.Len(events, 2)
	assert.Equal("setup", events[0].Stage)
	assert.Equal("started", events[0].Outcome)
	assert.Equal("setup", events[1].Stage)
	assert.Equal("failure", events[1].Outcome)
	assert.Contains(events[1].Message, "context canceled")
}

func TestSetupUsesConfiguredWorktreeBasePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	hooksDir, err := effectiveHooksDir(t.Context(), localRepo)
	require.NoError(err)
	binDir := t.TempDir()
	require.NoError(os.WriteFile(
		filepath.Join(binDir, "roborev"), []byte("#!/bin/sh\nexit 99\n"), 0o755,
	))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	require.NoError(mgr.SetupWithOptions(
		t.Context(), ws, SetupOptions{RoborevInitManagedClones: true},
	))
	assert.NoFileExists(filepath.Join(hooksDir, "post-commit"))
	assert.NoFileExists(filepath.Join(hooksDir, "post-rewrite"))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal("feature/thing", got.WorkspaceBranch)

	listOutput := string(runWorkspaceTestGit(t, localRepo, "worktree", "list", "--porcelain"))
	canonicalWorktreePath, err := filepath.EvalSymlinks(ws.WorktreePath)
	require.NoError(err)
	assert.Contains(listOutput, "worktree "+canonicalWorktreePath)

	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	sourceSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/feature/thing",
	)))
	assert.Equal(sourceSHA, headSHA)

	// Agent context is launch-scoped: setup must not create agent files.
	assert.NoFileExists(filepath.Join(ws.WorktreePath, "AGENTS.override.md"))
	assert.NoFileExists(filepath.Join(ws.WorktreePath, "CLAUDE.local.md"))
	status := strings.TrimSpace(string(runWorkspaceTestGit(t, ws.WorktreePath, "status", "--porcelain")))
	assert.Empty(status)
}

func TestSetupCreatesPRWorktreeFromForkStyleWorktreeBase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")
	tmuxScript, _ := writeRecorderScript(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal("feature/thing", got.WorkspaceBranch)
	trackingRef := remoteTrackingRef("upstream", "feature/thing")
	_, exists, err := gitRefSHA(t.Context(), localRepo, trackingRef)
	require.NoError(err)
	assert.True(exists)
	remote, err := gitConfigValue(
		t.Context(), ws.WorktreePath, "branch.feature/thing.remote",
	)
	require.NoError(err)
	assert.Equal(originRemoteName, remote)
}

func TestSetupWithOptionsConfirmsRoborevBeforeTerminal(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{name: "registered repo without review jobs", response: "registered"},
		{name: "malformed inventory", response: "malformed", wantErr: "invalid daemon response"},
		{name: "missing total count", response: "missing total count", wantErr: "invalid daemon response"},
		{name: "absent registration", response: "absent", wantErr: "workspace is absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", "")
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			d := openTestDB(t)
			_, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
				t, "feature/source",
			)
			remote := "http://" + platformHost + "/acme/widget.git"
			repoID := seedRepo(t, d, platformHost, "acme", "widget")
			require.NoError(d.UpdateRepoProviderMetadata(
				ctx, repoID, db.RepoProviderMetadata{
					CloneURL:      remote,
					DefaultBranch: "main",
				},
			))

			clones := gitclone.New(t.TempDir(), nil)
			cloneCtx := gitclone.WithRepositoryIdentity(ctx, "repo-acme-widget")
			require.NoError(clones.EnsureClone(
				cloneCtx, "github", platformHost, "acme", "widget", remote,
			))
			cloneDir, cloneErr := clones.ClonePathForContext(
				cloneCtx, "github", platformHost, "acme", "widget",
			)
			require.NoError(cloneErr)
			originalHook := []byte("#!/bin/sh\n# existing security hook\n")
			postCommitPath := filepath.Join(cloneDir, "hooks", "post-commit")
			require.NoError(os.WriteFile(postCommitPath, originalHook, 0o755))
			siblingPath := filepath.Join(t.TempDir(), "sibling")
			runWorkspaceTestGit(
				t, cloneDir, "worktree", "add", "-b", "sibling", siblingPath, "main",
			)

			mgr := NewManager(d, t.TempDir())
			mgr.SetClones(clones)
			tmuxScript, tmuxRecord := writeRecorderScript(t)
			mgr.SetTmuxCommand([]string{tmuxScript})

			binDir := t.TempDir()
			roborevInvocations := filepath.Join(t.TempDir(), "roborev-invocations")
			t.Setenv("ROBOREV_INVOCATIONS", roborevInvocations)
			require.NoError(os.WriteFile(filepath.Join(binDir, "roborev"), []byte(`#!/bin/sh
set -eu
printf 'run\n' >> "$ROBOREV_INVOCATIONS"
hooks="$(git -C "$PWD" rev-parse --path-format=absolute --git-path hooks)"
mkdir -p "$hooks"
printf '\n# roborev post-commit hook\n' >> "$hooks/post-commit"
printf '#!/bin/sh\n# roborev post-rewrite hook\n' > "$hooks/post-rewrite"
chmod +x "$hooks/post-commit" "$hooks/post-rewrite"
`), 0o755))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			var ws *Workspace
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch tt.response {
				case "registered":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"repos":       []map[string]string{{"root_path": ws.WorktreePath}},
						"total_count": 0,
					})
				case "malformed":
					_, _ = w.Write([]byte("{"))
				case "missing total count":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"repos": []map[string]string{{"root_path": ws.WorktreePath}},
					})
				case "absent":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"repos":       []map[string]string{},
						"total_count": 0,
					})
				}
			}))
			t.Cleanup(server.Close)
			mgr.SetRoborevEndpoint(server.URL)
			var invalidations atomic.Int32
			mgr.SetRoborevRepositoryInvalidator(func() { invalidations.Add(1) })

			var err error
			ws, err = mgr.CreateAdHoc(
				ctx, "github", platformHost, "acme", "widget",
				CreateAdHocOptions{BranchName: "feature/hook-test"},
			)
			require.NoError(err)
			require.NotNil(ws)

			err = mgr.SetupWithOptions(ctx, ws, SetupOptions{RoborevInitManagedClones: true})
			stored, getErr := d.GetWorkspace(ctx, ws.ID)
			require.NoError(getErr)
			require.NotNil(stored)
			if tt.wantErr != "" {
				require.ErrorContains(err, tt.wantErr)
				assert.Equal("error", stored.Status)
				assert.NoFileExists(tmuxRecord)
				assert.Zero(invalidations.Load())
				content, readErr := os.ReadFile(postCommitPath)
				require.NoError(readErr)
				assert.Equal(originalHook, content)
				assert.NoFileExists(filepath.Join(cloneDir, "hooks", "post-rewrite"))
				_, _, configErr := gitcmd.New().Run(
					ctx, cloneDir, nil,
					"config", "--local", "--get-all", "core.hooksPath",
				)
				require.Error(configErr)
				resolved, resolveErr := effectiveHooksDir(ctx, siblingPath)
				require.NoError(resolveErr)
				canonicalHooks, canonicalErr := canonicalFilesystemPath(
					filepath.Join(cloneDir, "hooks"),
				)
				require.NoError(canonicalErr)
				assert.Equal(canonicalHooks, resolved)
				return
			}

			require.NoError(err)
			assert.Equal("ready", stored.Status)
			assert.FileExists(tmuxRecord)
			assert.Equal(int32(1), invalidations.Load())
			content, readErr := os.ReadFile(postCommitPath)
			require.NoError(readErr)
			assert.Contains(string(content), "existing security hook")
			assert.Contains(string(content), "roborev post-commit hook")

			require.NoError(mgr.SetupWithOptions(
				ctx, stored, SetupOptions{RoborevInitManagedClones: true},
			))
			invocations, readErr := os.ReadFile(roborevInvocations)
			require.NoError(readErr)
			assert.Len(strings.Fields(string(invocations)), 2)
		})
	}
}

func TestManagedRepositoryHookSetupSerializesRegistration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	require := require.New(t)
	ctx := t.Context()
	_, remote := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(
		t, "feature/source",
	)
	clones := gitclone.New(t.TempDir(), nil)
	require.NoError(clones.EnsureClone(
		ctx, "github", "github.com", "acme", "widget", remote,
	))
	cloneDir, err := clones.ClonePathForContext(
		ctx, "github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	worktreePath := filepath.Join(t.TempDir(), "worktree")
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "add", "-b", "serialization", worktreePath, "main",
	)

	d := openTestDB(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetClones(clones)
	ws := &Workspace{
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		WorktreePath: worktreePath,
	}
	binDir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(binDir, "roborev"), []byte(`#!/bin/sh
set -eu
hooks="$(git -C "$PWD" rev-parse --path-format=absolute --git-path hooks)"
printf '#!/bin/sh\n# roborev post-commit hook\n' > "$hooks/post-commit"
printf '#!/bin/sh\n# roborev post-rewrite hook\n' > "$hooks/post-rewrite"
chmod +x "$hooks/post-commit" "$hooks/post-rewrite"
`), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	confirmationStarted := make(chan struct{})
	releaseConfirmation := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseConfirmation) }) }
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(confirmationStarted)
			<-releaseConfirmation
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repos":       []map[string]string{{"root_path": worktreePath}},
			"total_count": 1,
		})
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	mgr.SetRoborevEndpoint(server.URL)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- mgr.setupManagedRepositoryHooks(ctx, cloneDir, ws)
	}()
	select {
	case <-confirmationStarted:
	case <-time.After(5 * time.Second):
		require.FailNow("first setup did not begin registration confirmation")
	}

	secondCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	err = mgr.setupManagedRepositoryHooks(secondCtx, cloneDir, ws)
	require.ErrorIs(err, context.DeadlineExceeded)
	require.Equal(int32(1), requests.Load())
	observedAt := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-old",
		Owner: "acme", Name: "widget",
	}, observedAt)
	require.NoError(err)
	_, _, err = d.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com", PlatformRepoID: "repo-new",
		Owner: "acme", Name: "widget",
	}, observedAt.Add(time.Hour))
	require.NoError(err)
	release()
	require.ErrorContains(<-firstDone, "historical occupants")
	require.NoFileExists(filepath.Join(cloneDir, "hooks", "post-commit"))
	require.NoFileExists(filepath.Join(cloneDir, "hooks", "post-rewrite"))
}

func TestSetupReusesExistingWorkspaceWorktree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, tmuxRecord := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	existingBranch := syntheticPRWorktreeBranch(42)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "-b", existingBranch, "HEAD",
	)

	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal(existingBranch, got.WorkspaceBranch)
	headBranch := strings.TrimSpace(string(runWorkspaceTestGit(
		t, ws.WorktreePath, "branch", "--show-current",
	)))
	assert.Equal(existingBranch, headBranch)
	argvs := readRecorderArgv(t, tmuxRecord)
	require.NotEmpty(argvs)
	assert.Contains(argvs[0], ws.WorktreePath)
}

func TestReuseExistingWorkspaceWorktreeRechecksSymlinkAfterLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	readyForRepoLock := make(chan struct{})
	continueToRepoLock := make(chan struct{})
	var continueOnce sync.Once
	continueReuse := func() {
		continueOnce.Do(func() { close(continueToRepoLock) })
	}
	defer continueReuse()
	mgr.beforeExistingWorktreeRepoLock = func() {
		close(readyForRepoLock)
		<-continueToRepoLock
	}
	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	existingBranch := syntheticPRWorktreeBranch(42)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "-b", existingBranch, "HEAD",
	)
	metadataDir, err := worktreeGitDir(t.Context(), ws.WorktreePath)
	require.NoError(err)

	type reuseResult struct {
		reused bool
		err    error
	}
	done := make(chan reuseResult, 1)
	go func() {
		_, reused, reuseErr := mgr.reuseExistingWorkspaceWorktree(t.Context(), ws, nil)
		done <- reuseResult{reused: reused, err: reuseErr}
	}()

	select {
	case <-readyForRepoLock:
	case result := <-done:
		require.FailNowf(
			"worktree reuse completed before repository lock acquisition",
			"reused=%t err=%v", result.reused, result.err,
		)
	case <-time.After(5 * time.Second):
		require.FailNow("worktree reuse did not reach repository lock acquisition")
	}
	targetPath := filepath.Join(wtDir, "replacement-target")
	require.NoError(os.Rename(ws.WorktreePath, targetPath))
	require.NoError(os.Symlink(targetPath, ws.WorktreePath))
	continueReuse()

	select {
	case result := <-done:
		require.Error(result.err)
		assert.False(result.reused)
	case <-time.After(5 * time.Second):
		require.FailNow("worktree reuse did not finish after lock release")
	}
	pathInfo, err := os.Lstat(ws.WorktreePath)
	require.NoError(err)
	assert.NotZero(pathInfo.Mode() & os.ModeSymlink)
	_, err = os.Lstat(filepath.Join(metadataDir, workspaceOwnershipMarkerFile))
	assert.ErrorIs(err, os.ErrNotExist)
}

// A retried setup must recognize the uniquified synthetic branch as its own,
// otherwise the workspace whose first attempt hit a name collision can never be
// set up again.
func TestSetupReusesExistingUniquifiedSyntheticPRWorktree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	existingBranch := syntheticPRWorktreeBranch(42) + "-2"
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "-b", existingBranch, "HEAD",
	)

	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal(existingBranch, got.WorkspaceBranch)
}

func TestSetupDoesNotReuseUnconfiguredMatchingOriginWorktree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath,
		"-b", syntheticPRWorktreeBranch(42), "HEAD",
	)

	err = mgr.Setup(t.Context(), ws)

	var recoveryErr *WorkspaceDirectoryRecoveryError
	require.ErrorAs(err, &recoveryErr)
	assert.Equal(WorkspaceDirectoryRepositoryMismatch, recoveryErr.Reason)
	got, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	assert.Equal(workspaceBranchUnknown, got.WorkspaceBranch)
}

func TestSetupRejectsExistingLocalBaseWorktreeWithExecutableConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath,
		"-b", syntheticPRWorktreeBranch(42), "HEAD",
	)
	runWorkspaceTestGit(t, localRepo, "config", "extensions.worktreeConfig", "true")
	runWorkspaceTestGit(
		t, ws.WorktreePath,
		"config", "--worktree", "core.fsmonitor", "demo-fsmonitor",
	)

	err = mgr.Setup(t.Context(), ws)

	require.Error(err)
	assert.Contains(err.Error(), "local git config")
	assert.Contains(err.Error(), "core.fsmonitor")
	got, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	assert.Equal(workspaceBranchUnknown, got.WorkspaceBranch)
}

func TestSetupRejectsExistingSyntheticPRWorktreeOnStaleHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	existingBranch := syntheticPRWorktreeBranch(42)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "-b", existingBranch, "HEAD",
	)

	require.NoError(os.WriteFile(
		filepath.Join(localRepo, "new-head.txt"), []byte("new head\n"), 0o644,
	))
	runWorkspaceTestGit(t, localRepo, "add", ".")
	runWorkspaceTestGit(t, localRepo, "commit", "-m", "new pr head")
	newHead := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "HEAD",
	)))
	runWorkspaceTestGit(
		t, localRepo, "push", remote,
		"HEAD:refs/heads/feature/thing",
	)
	runWorkspaceTestGit(t, remote, "update-server-info")

	err = mgr.Setup(t.Context(), ws)

	require.Error(err)
	assert.Contains(err.Error(), "not current workspace head")
	got, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Contains(*got.ErrorMessage, "not current workspace head")
	gotHead := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/feature/thing",
	)))
	assert.Equal(newHead, gotHead)
}

func TestSetupReusesExistingLocalBasePRHeadBranchWithoutManagingIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	runWorkspaceTestGit(
		t, localRepo,
		"branch", "feature/thing", "refs/remotes/origin/feature/thing",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "feature/thing",
	)

	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Empty(got.WorkspaceBranch)

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), got))
	exists, err := localBranchExists(t.Context(), localRepo, "feature/thing")
	require.NoError(err)
	assert.True(exists)
}

func TestSetupRejectsOrphanedWorkspacePathBeforeCreatingBranches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	ws, err := mgr.Create(
		t.Context(), "github", platformHost, "acme", "widget", 42,
	)
	require.NoError(err)
	require.NoError(os.MkdirAll(ws.WorktreePath, 0o755))
	staleGitDir := filepath.Join(t.TempDir(), "removed", "worktrees", "pr-42")
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, ".git"),
		[]byte("gitdir: "+staleGitDir+"\n"),
		0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "keep.txt"), []byte("preserve me\n"), 0o644,
	))

	err = mgr.Setup(t.Context(), ws)

	var recoveryErr *WorkspaceDirectoryRecoveryError
	if assert.ErrorAs(err, &recoveryErr) {
		assert.Equal(WorkspaceDirectoryNotLinkedWorktree, recoveryErr.Reason)
	}
	contents, readErr := os.ReadFile(filepath.Join(ws.WorktreePath, "keep.txt"))
	require.NoError(readErr)
	assert.Equal("preserve me\n", string(contents))
	for _, branch := range []string{ws.GitHeadRef, syntheticPRWorktreeBranch(42)} {
		exists, branchErr := localBranchExists(t.Context(), localRepo, branch)
		require.NoError(branchErr)
		assert.False(exists, "setup must not create branch %q", branch)
	}
}

func TestSetupRejectsExistingPRWorktreeOnUnexpectedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", ws.WorktreePath, "-b", "wrong/main", "main",
	)

	err = mgr.Setup(t.Context(), ws)

	require.Error(err)
	got, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Contains(*got.ErrorMessage, "existing worktree branch")
	assert.Contains(*got.ErrorMessage, "wrong/main")
}

func TestValidateWorktreeBasePathRejectsLocalRemotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name      string
		remoteURL string
	}{
		{name: "absolute path", remoteURL: filepath.Join(t.TempDir(), "remote.git")},
		{name: "file URL", remoteURL: "file://" + filepath.ToSlash(filepath.Join(t.TempDir(), "remote.git"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
			runWorkspaceTestGit(
				t, localRepo, "remote", "set-url", "origin", tt.remoteURL,
			)

			got, err := ValidateWorktreeBasePath(
				t.Context(), localRepo, "github.com", "acme", "widget", false)

			require.Empty(got.Path)
			require.Error(err)
			assert.Contains(err.Error(), "no git remote matches configured repository")
		})
	}
}

func TestValidateWorktreeBasePathResolvesCanonicalRemoteByIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, platformHost, "acme", "widget", false,
	)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	assert.Equal(canonicalLocalRepo, base.Path)
	assert.Equal("upstream", base.Remote)
}

func TestValidateWorktreeBasePathRejectsAmbiguousCanonicalRemotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	canonicalURL := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "remote", "get-url", "upstream",
	)))
	runWorkspaceTestGit(t, localRepo, "remote", "add", "mirror", canonicalURL)

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, platformHost, "acme", "widget", false,
	)

	require.Empty(base.Path)
	require.Error(err)
	assert.Contains(err.Error(), `git remotes "mirror" and "upstream" both match`)
	assert.NotContains(err.Error(), canonicalURL)
}

func TestValidateWorktreeBasePathRejectsMissingCanonicalRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	runWorkspaceTestGit(t, localRepo, "remote", "remove", "upstream")

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, platformHost, "acme", "widget", false,
	)

	require.Empty(base.Path)
	require.Error(err)
	assert.Contains(err.Error(), "no git remote matches configured repository")
	assert.Contains(err.Error(), "origin")
}

func TestValidateWorktreeBasePathRejectsForeignRemoteWritingBaseNamespace(t *testing.T) {
	for _, refspec := range []string{
		"+refs/heads/*:refs/remotes/upstream/*",
		"+refs/tags/*:refs/remotes/upstream/tags/*",
		"+refs/heads/*:refs/remotes/*",
		"+refs/heads/*:refs/*",
		"+refs/heads/main:refs/remotes/upstream",
		"+refs/heads/*:refs/remotes/upstr*",
	} {
		t.Run(refspec, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
				t, "feature/thing",
			)
			runWorkspaceTestGit(
				t, localRepo, "config", "--add", "remote.origin.fetch", refspec,
			)

			base, err := ValidateWorktreeBasePath(
				t.Context(), localRepo, platformHost, "acme", "widget", false,
			)

			require.Empty(base.Path)
			require.Error(err)
			assert.Contains(err.Error(), `remote "origin" fetch refspec`)
			assert.Contains(err.Error(), `"upstream" tracking namespace`)
		})
	}
}

func TestValidateWorktreeBasePathAllowsForeignRemoteWithSimilarNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	runWorkspaceTestGit(
		t, localRepo, "config", "--add", "remote.origin.fetch",
		"+refs/heads/*:refs/remotes/upstreamish/*",
	)

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, platformHost, "acme", "widget", false,
	)

	require.NoError(err)
	assert.Equal("upstream", base.Remote)
}

func TestValidateWorktreeBasePathRejectsUnsafeCanonicalRemoteName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo, _, platformHost := setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	runWorkspaceTestGit(
		t, localRepo, "config", "--add", "remote.-bad.url",
		"https://"+platformHost+"/acme/widget.git",
	)

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, platformHost, "acme", "widget", false,
	)

	require.Empty(base.Path)
	require.Error(err)
	assert.Contains(err.Error(), `git remote name "-bad" is unsafe`)
}

func TestValidateWorktreeBasePathAllowsUnsafeUnrelatedRemoteName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo, "config", "--add", "remote.-bad.url",
		"https://github.com/other/widget.git",
	)

	base, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false,
	)

	require.NoError(err)
	assert.Equal("origin", base.Remote)
}

func TestValidateWorktreeBasePathRejectsExecutableLocalConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "filter process", key: "filter.demo.process", value: "demo-filter"},
		{name: "filter smudge", key: "filter.demo.smudge", value: "demo-smudge"},
		{name: "filter clean", key: "filter.demo.clean", value: "demo-clean"},
		{name: "fsmonitor", key: "core.fsmonitor", value: "demo-fsmonitor"},
		{name: "alternate refs command", key: "core.alternateRefsCommand", value: "demo-alternates"},
		{name: "askpass", key: "core.askPass", value: "demo-askpass"},
		{name: "git proxy", key: "core.gitProxy", value: "demo-proxy"},
		{name: "ssh command", key: "core.sshCommand", value: "demo-ssh"},
		{name: "credential helper", key: "credential.helper", value: "!demo-helper"},
		{name: "diff external", key: "diff.external", value: "demo-diff"},
		{name: "diff driver command", key: "diff.demo.command", value: "demo-command"},
		{name: "diff textconv", key: "diff.demo.textconv", value: "demo-textconv"},
		{name: "fetch recurse submodules", key: "fetch.recurseSubmodules", value: "true"},
		{name: "http proxy", key: "http.proxy", value: "http://127.0.0.1:1"},
		{name: "http url proxy", key: "http.https://github.com.proxy", value: "http://127.0.0.1:1"},
		{name: "http ssl verify", key: "http.sslVerify", value: "false"},
		{name: "http extra header", key: "http.extraHeader", value: "Authorization: bearer secret"},
		{name: "http cookie file", key: "http.cookieFile", value: filepath.Join(t.TempDir(), "cookies")},
		{name: "remote proxy", key: "remote.origin.proxy", value: "http://127.0.0.1:1"},
		{name: "submodule recurse", key: "submodule.recurse", value: "true"},
		{name: "url rewrite", key: "url.https://example.invalid/.insteadOf", value: "https://github.com/"},
		{name: "include path", key: "include.path", value: filepath.Join(t.TempDir(), "config")},
		{name: "conditional include", key: "includeIf.gitdir:~/demo/.path", value: filepath.Join(t.TempDir(), "config")},
		{name: "protocol allow", key: "protocol.ext.allow", value: "always"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
			runWorkspaceTestGit(t, localRepo, "config", tt.key, tt.value)

			got, err := ValidateWorktreeBasePath(
				t.Context(), localRepo, "github.com", "acme", "widget", false)

			require.Empty(got.Path)
			require.Error(err)
			assert.Contains(
				strings.ToLower(err.Error()), strings.ToLower(tt.key),
			)
			assert.Contains(err.Error(), "may execute or rewrite git commands")
		})
	}
}

func TestValidateWorktreeBasePathAcceptsConfiguredHooksPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	hooksPath := t.TempDir()
	runWorkspaceTestGit(t, localRepo, "config", "core.hooksPath", hooksPath)
	commonDir := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "--path-format=absolute", "--git-common-dir",
	)))
	hookPath := filepath.Join(commonDir, "hooks", "post-commit")
	require.NoError(os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	assert.Equal(canonicalLocalRepo, got.Path)
}

func TestValidateWorktreeBasePathRejectsUnsafeBaseRemoteSchemes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		name       string
		remoteURL  string
		wantScheme string
	}{
		{
			name:       "git protocol",
			remoteURL:  "git://github.com/acme/widget.git",
			wantScheme: "git",
		},
		{
			name:       "plain http",
			remoteURL:  "http://github.com/acme/widget.git",
			wantScheme: "http",
		},
		{
			name:       "http with embedded credentials",
			remoteURL:  "http://oauth2:secret-token@github.com/acme/widget.git",
			wantScheme: "http",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
			runWorkspaceTestGit(
				t, localRepo, "remote", "set-url", "origin", tt.remoteURL,
			)

			got, err := ValidateWorktreeBasePath(
				t.Context(), localRepo, "github.com", "acme", "widget", false)

			require.Empty(got.Path)
			require.Error(err)
			assert.Contains(
				err.Error(),
				fmt.Sprintf(
					"\"origin\" remote scheme %q is not allowed (host %q)",
					tt.wantScheme, "github.com",
				),
			)
			// The validation error is persisted as workspace error state and
			// served through the API, so credentials must never appear.
			assert.NotContains(err.Error(), "secret-token")
			assert.NotContains(err.Error(), tt.remoteURL)
		})
	}
}

func TestValidateWorktreeBasePathAcceptsLoopbackHTTPOrigin(t *testing.T) {
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo, "remote", "set-url", "origin",
		"http://127.0.0.1/acme/widget.git",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "127.0.0.1", "acme", "widget", false)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	require.Equal(canonicalLocalRepo, got.Path)
}

func TestValidateWorktreeBasePathAcceptsExplicitlyAllowedHTTPOrigin(t *testing.T) {
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo, "remote", "set-url", "origin",
		"http://gitea.example.test:3000/acme/widget.git",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "gitea.example.test:3000", "acme", "widget", true,
	)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	require.Equal(canonicalLocalRepo, got.Path)
}

func TestValidateWorktreeBasePathAcceptsSCPStyleSSHOrigin(t *testing.T) {
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo, "remote", "set-url", "origin",
		"git@github.com:acme/widget.git",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	require.Equal(canonicalLocalRepo, got.Path)
}

func TestValidateWorktreeBasePathCanonicalizesSymlinkPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	parentLink := filepath.Join(t.TempDir(), "parent-link")
	require.NoError(os.Symlink(filepath.Dir(localRepo), parentLink))
	tests := []struct {
		name string
		path string
	}{
		{
			name: "final component",
			path: func() string {
				linkPath := filepath.Join(t.TempDir(), "repo-link")
				require.NoError(os.Symlink(localRepo, linkPath))
				return linkPath
			}(),
		},
		{
			name: "parent component",
			path: filepath.Join(parentLink, filepath.Base(localRepo)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateWorktreeBasePath(
				t.Context(), tt.path, "github.com", "acme", "widget", false)

			require.NoError(err)
			assert.Equal(canonicalLocalRepo, got.Path)
		})
	}
}

func TestValidateWorktreeBasePathRejectsAdditionalBaseRemoteURLs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo, "config", "--add", "remote.origin.url",
		"https://github.com/evil/widget.git",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.Empty(got.Path)
	require.Error(err)
	assert.Contains(err.Error(), `"origin" remote does not match repository`)
}

func TestValidateWorktreeBasePathRejectsUnsafeBaseFetchRefspec(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo,
		"config", "--unset-all", "remote.origin.fetch",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"config", "--add", "remote.origin.fetch",
		"+refs/heads/*:refs/heads/*",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.Empty(got.Path)
	require.Error(err)
	assert.Contains(err.Error(), "\"origin\" fetch refspec")
	assert.Contains(err.Error(), "may update unsafe refs")
}

func TestValidateWorktreeBasePathAcceptsSingleBranchBaseFetchRefspec(t *testing.T) {
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, localRepo,
		"config", "--unset-all", "remote.origin.fetch",
	)
	runWorkspaceTestGit(
		t, localRepo,
		"config", "--add", "remote.origin.fetch",
		"+refs/heads/main:refs/remotes/origin/main",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.NoError(err)
	canonicalLocalRepo, err := filepath.EvalSymlinks(localRepo)
	require.NoError(err)
	require.Equal(canonicalLocalRepo, got.Path)
}

func TestValidateWorktreeBasePathRejectsBareRepositories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	bareRepo := filepath.Join(dir, "repo.git")
	runWorkspaceTestGit(t, dir, "init", "--bare", bareRepo)
	runWorkspaceTestGit(
		t, bareRepo, "config", "remote.origin.url",
		"https://github.com/acme/widget.git",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), bareRepo, "github.com", "acme", "widget", false)

	require.Empty(got.Path)
	require.Error(err)
	assert.Contains(err.Error(), "path is not a git worktree")
}

func TestValidateWorktreeBasePathRejectsExecutableWorktreeConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(t, localRepo, "config", "extensions.worktreeConfig", "true")
	runWorkspaceTestGit(
		t, localRepo, "config", "--worktree",
		"filter.demo.clean", "demo-clean",
	)

	got, err := ValidateWorktreeBasePath(
		t.Context(), localRepo, "github.com", "acme", "widget", false)

	require.Empty(got.Path)
	require.Error(err)
	assert.Contains(err.Error(), "filter.demo.clean")
	assert.Contains(err.Error(), "may execute or rewrite git commands")
}

func TestCreateIssueUsesProviderQualifiedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "forge.example.com",
		PlatformRepoID: "repo-github-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "forge.example.com",
		PlatformRepoID: "repo-gitlab-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	seedIssue(t, d, gitlabRepoID, 7, "GitLab issue")

	mgr := newTestManager(t, d, t.TempDir())
	ws, err := mgr.CreateIssue(
		ctx, "forge.example.com", "acme", "widget", 7,
		CreateIssueOptions{Provider: "gitlab"},
	)

	require.NoError(err)
	require.NotNil(ws)
	assert.Equal("gitlab", ws.Platform)
}

func TestCreateIssueUsesProviderCloneURLForNamespacedManagedClone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	_, remote := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(
		t, "feature/thing",
	)

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "repo-gitlab-project",
		Owner:          "group",
		Name:           "project",
	})
	require.NoError(err)
	require.NoError(d.UpdateRepoProviderMetadata(
		ctx, repoID, db.RepoProviderMetadata{
			CloneURL:      remote,
			DefaultBranch: "main",
		},
	))
	seedIssue(t, d, repoID, 11, "GitLab issue")

	clones := gitclone.New(filepath.Join(t.TempDir(), "clones"), nil)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetClones(clones)

	ws, err := mgr.CreateIssue(
		ctx, "gitlab.example.com", "group", "project", 11,
		CreateIssueOptions{Provider: "gitlab"},
	)

	require.NoError(err)
	require.NotNil(ws)
	assert.Equal("gitlab", ws.Platform)
	cloneDir, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(ctx, "repo-gitlab-project"),
		"gitlab", "gitlab.example.com", "group", "project",
	)
	require.NoError(err)
	assert.DirExists(cloneDir)
	assert.Equal(
		remote,
		strings.TrimSpace(string(runWorkspaceTestGit(
			t, cloneDir, "config", "--get", "remote.origin.url",
		))),
	)
}

func TestCreateIssueClonesExplicitlyAllowedGiteaHTTPRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	_, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	cloneURL := "http://" + platformHost + "/acme/widget.git"

	repoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitea",
		PlatformHost:   platformHost,
		PlatformRepoID: "repo-gitea-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	require.NoError(d.UpdateRepoProviderMetadata(
		ctx, repoID, db.RepoProviderMetadata{
			CloneURL:      cloneURL,
			DefaultBranch: "main",
		},
	))
	seedIssue(t, d, repoID, 11, "Gitea issue")

	clones := gitclone.New(filepath.Join(t.TempDir(), "clones"), nil)
	clones.SetAllowInsecureHTTP("gitea", platformHost, true)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetClones(clones)

	ws, err := mgr.CreateIssue(
		ctx, platformHost, "acme", "widget", 11,
		CreateIssueOptions{Provider: "gitea"},
	)

	require.NoError(err)
	require.NotNil(ws)
	cloneDir, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(ctx, "repo-gitea-widget"),
		"gitea", platformHost, "acme", "widget",
	)
	require.NoError(err)
	assert.DirExists(cloneDir)
	assert.Equal(cloneURL, strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "config", "--get", "remote.origin.url",
	))))
}

func TestBranchInspectionPartitionsManagedCloneByProviderIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	d := openTestDB(t)
	_, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	remote := "http://" + platformHost + "/acme/widget.git"
	repoID := seedRepo(t, d, platformHost, "acme", "widget")

	clones := gitclone.New(t.TempDir(), nil)
	clones.SetAllowInsecureHTTP("github", platformHost, true)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetClones(clones)

	repo := workspaceRepoRef{
		ID: repoID, Platform: "github", PlatformHost: platformHost,
		ProviderID: "provider-repo-a", Owner: "acme", Name: "widget",
		RemoteURL: remote,
	}
	firstDir, ok, localBase, err := mgr.branchInspectionDir(ctx, repo)
	require.NoError(err)
	require.True(ok)
	assert.False(localBase)

	repo.ProviderID = "provider-repo-b"
	secondDir, ok, localBase, err := mgr.branchInspectionDir(ctx, repo)
	require.NoError(err)
	require.True(ok)
	assert.False(localBase)
	assert.NotEqual(firstDir, secondDir)
}

func TestWorkspaceSetupGitDirRemovesCloneWhenRouteChangesDuringClone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := openTestDB(t)
	_, remote := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(
		t, "feature/thing",
	)
	repoID := seedRepo(t, database, "github.com", "acme", "widget")
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			CloneURL: remote, DefaultBranch: "main",
		},
	))
	clones := gitclone.New(t.TempDir(), nil)
	manager := newTestManager(t, database, t.TempDir())
	manager.SetClones(clones)
	workspace := &Workspace{
		RepoID: repoID, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
	}
	routeChanged := errors.New("repository route changed")
	validations := 0

	_, err := manager.workspaceSetupGitDir(
		t.Context(), workspace, "", nil,
		func(context.Context) error {
			validations++
			if validations == 1 {
				return nil
			}
			return routeChanged
		},
	)

	require.ErrorIs(err, routeChanged)
	assert.Equal(2, validations)
	clonePath, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	assert.NoDirExists(clonePath)
}

func TestCreateUsesProviderQualifiedRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	worktreeDir := t.TempDir()

	_, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "forge.example.com",
		PlatformRepoID: "repo-github-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "forge.example.com",
		PlatformRepoID: "repo-gitlab-widget",
		Owner:          "acme",
		Name:           "widget",
	})
	require.NoError(err)
	seedMR(t, d, gitlabRepoID, 42, "feature/gitlab")

	mgr := newTestManager(t, d, worktreeDir)
	ws, err := mgr.Create(
		ctx, "gitlab", "forge.example.com", "acme", "widget", 42,
	)

	require.NoError(err)
	require.NotNil(ws)
	assert.Equal("gitlab", ws.Platform)
	assert.Equal("feature/gitlab", ws.GitHeadRef)
	assert.Equal(
		filepath.Join(
			worktreeDir, "gitlab", "forge.example.com", "acme", "widget",
			fmt.Sprintf("repo-%d", gitlabRepoID), "pr-42",
		),
		ws.WorktreePath,
	)
}

func TestSetupUsesManagedCloneForForkPRWithConfiguredWorktreeBasePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()
	cloneBaseDir := t.TempDir()

	const (
		host     = "github.com"
		owner    = "acme"
		name     = "widget"
		prNumber = 245
		branch   = "fork/thing"
	)
	repoID := seedRepo(t, d, host, owner, name)
	remote, pullSHA := setupRemoteForForkPRWorktreeTest(
		t, branch, prNumber,
	)
	seedMRWithHeadRepo(
		t, d, repoID, prNumber, branch,
		"https://github.com/acme/widget.git",
	)
	clones := gitclone.New(cloneBaseDir, nil)
	cloneDir, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", host, owner, name,
	)
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(cloneDir), 0o755))
	runWorkspaceTestGit(t, cloneBaseDir, "clone", "--bare", remote, cloneDir)
	runWorkspaceTestGit(
		t, cloneDir, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(
		t, cloneDir, "config", "--add",
		"url."+remote+".insteadOf", "https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(t, cloneDir, "update-ref", "-d", "refs/heads/"+branch)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, branch)
	tmuxScript, _ := writeRecorderScript(t)

	mgr := newTestManager(t, d, wtDir)
	mgr.SetClones(clones)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	spec, err := databaseLaunchSpecResolver{db: d}.ResolveWorkspaceLaunchSpec(
		t.Context(), providerplane.WorkspaceLaunchRequest{
			Repository: providerplane.RepositoryRoute{
				Provider: "github", PlatformHost: host, Owner: owner, Name: name,
			},
			ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: prNumber,
		},
	)
	require.NoError(err)
	spec.Pull.HeadRepoKind = "fork"
	spec.Pull.HeadRepoCloneURL = remote
	ws, err := mgr.CreateFromLaunchSpec(t.Context(), spec)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal(branch, got.WorkspaceBranch)

	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(pullSHA, headSHA)

	canonicalWorktreePath, err := filepath.EvalSymlinks(ws.WorktreePath)
	require.NoError(err)
	managedList := string(runWorkspaceTestGit(
		t, cloneDir, "worktree", "list", "--porcelain",
	))
	assert.Contains(managedList, "worktree "+canonicalWorktreePath)
	localList := string(runWorkspaceTestGit(
		t, localRepo, "worktree", "list", "--porcelain",
	))
	assert.NotContains(localList, "worktree "+canonicalWorktreePath)
}

func TestSetupFetchesConfiguredWorktreeBasePathBeforeAdd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	const branch = "feature/fetch-before-add"
	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, branch,
	)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 42, branch)

	remoteWork := filepath.Join(t.TempDir(), "remote-work")
	runWorkspaceTestGit(t, t.TempDir(), "clone", remote, remoteWork)
	runWorkspaceTestGit(t, remoteWork, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, remoteWork, "config", "user.name", "Test")
	runWorkspaceTestGit(t, remoteWork, "checkout", branch)
	require.NoError(os.WriteFile(
		filepath.Join(remoteWork, "fresh.txt"), []byte("fresh\n"), 0o644,
	))
	runWorkspaceTestGit(t, remoteWork, "add", ".")
	runWorkspaceTestGit(t, remoteWork, "commit", "-m", "fresh branch commit")
	expectedSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, remoteWork, "rev-parse", "HEAD",
	)))
	runWorkspaceTestGit(t, remoteWork, "push", "origin", "HEAD:refs/heads/"+branch)
	runWorkspaceTestGit(t, remote, "update-server-info")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.Create(t.Context(), "github", platformHost, "acme", "widget", 42)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(expectedSHA, headSHA)
}

func TestSetupRefreshesConfiguredWorktreeBaseOriginHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/thing",
	)
	runWorkspaceTestGit(t, localRepo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedIssue(t, d, repoID, 7, "")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := newTestManager(t, d, wtDir)
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7, CreateIssueOptions{Provider: "github"},
	)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal("kenn-forge/issue-7", got.WorkspaceBranch)
	ref := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "symbolic-ref", "refs/remotes/origin/HEAD",
	)))
	assert.Equal("refs/remotes/origin/main", ref)
}

func TestFetchWorkspaceBaseRequiresOriginHeadOnlyForIssueWorkspaces(t *testing.T) {
	require := require.New(t)

	const branch = "feature/no-head"
	root := t.TempDir()
	remote := filepath.Join(root, "acme", "widget.git")
	localRepo := filepath.Join(root, "repo")
	require.NoError(os.MkdirAll(filepath.Dir(remote), 0o755))
	runWorkspaceTestGit(t, root, "init", "--bare", "--initial-branch=trunk", remote)
	runWorkspaceTestGit(t, root, "init", "--initial-branch=trunk", localRepo)
	runWorkspaceTestGit(t, localRepo, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, localRepo, "config", "user.name", "Test")
	runWorkspaceTestGit(t, localRepo, "remote", "add", "origin", remote)
	require.NoError(os.WriteFile(
		filepath.Join(localRepo, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, localRepo, "add", ".")
	runWorkspaceTestGit(t, localRepo, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, localRepo, "push", "origin", "HEAD:refs/heads/trunk")
	runWorkspaceTestGit(t, localRepo, "push", "origin", "HEAD:refs/heads/"+branch)
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)
	runWorkspaceTestGit(
		t, localRepo, "remote", "set-url", "origin",
		server.URL+"/acme/widget.git",
	)
	runWorkspaceTestGit(t, remote, "update-server-info")
	runWorkspaceTestGit(t, localRepo, "fetch", "--prune", "origin")
	runWorkspaceTestGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/missing")
	runWorkspaceTestGit(t, remote, "update-server-info")
	runWorkspaceTestGit(
		t, localRepo,
		"update-ref", "--no-deref", "-d", "refs/remotes/origin/HEAD",
	)

	require.NoError(fetchWorkspaceBaseWithGit(t.Context(), runGitWithoutHooks, localRepo, "origin", false))
	require.Error(fetchWorkspaceBaseWithGit(t.Context(), runGitWithoutHooks, localRepo, "origin", true))
}

func TestAddAndRefreshPRWorktreeFastForwardLocalBaseBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/base-sync"
	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, branch,
	)
	runWorkspaceTestGit(t, localRepo, "checkout", "--detach")
	runWorkspaceTestGit(t, remote, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, remote, "config", "user.name", "Test")
	firstBaseSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, remote, "commit-tree", "refs/heads/main^{tree}",
		"-p", "refs/heads/main", "-m", "advance base once",
	)))
	runWorkspaceTestGit(t, remote, "update-ref", "refs/heads/main", firstBaseSHA)
	runWorkspaceTestGit(t, remote, "update-server-info")

	d := openTestDB(t)
	repoID := seedRepo(t, d, platformHost, "acme", "widget")
	seedMR(t, d, repoID, 968, branch)
	mgr := newTestManager(t, d, t.TempDir())
	ws, err := mgr.Create(
		t.Context(), "github", platformHost, "acme", "widget", 968,
	)
	require.NoError(err)
	launchSpec, err := mgr.RequireWorkspaceLaunchSpec(t.Context(), ws)
	require.NoError(err)

	_, err = mgr.addWorktree(t.Context(), workspaceGitDir{
		path: localRepo, remote: originRemoteName, localBase: true,
	}, ws, workspaceGitFetchOptions{
		launchSpec: launchSpec,
	})
	require.NoError(err)
	localBaseSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	)))
	assert.Equal(firstBaseSHA, localBaseSHA)

	secondBaseSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, remote, "commit-tree", firstBaseSHA+"^{tree}",
		"-p", firstBaseSHA, "-m", "advance base twice",
	)))
	runWorkspaceTestGit(t, remote, "update-ref", "refs/heads/main", secondBaseSHA)
	runWorkspaceTestGit(t, remote, "update-server-info")

	_, err = mgr.refreshExistingWorkspaceWorktree(
		t.Context(), localRepo, originRemoteName, ws, launchSpec, nil,
	)
	require.NoError(err)
	localBaseSHA = strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	)))
	assert.Equal(secondBaseSHA, localBaseSHA)
}

func TestAddWorktreeRestoresBaseRefsWhenRouteChangesDuringFetch(t *testing.T) {
	require := require.New(t)
	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/route-fence",
	)
	originalSHA, exists, err := gitRefSHA(
		t.Context(), localRepo, "refs/remotes/origin/main",
	)
	require.NoError(err)
	require.True(exists)
	runWorkspaceTestGit(t, remote, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, remote, "config", "user.name", "Test")
	newSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, remote, "commit-tree", "refs/heads/main^{tree}",
		"-p", "refs/heads/main", "-m", "replacement base",
	)))
	runWorkspaceTestGit(t, remote, "update-ref", "refs/heads/main", newSHA)
	runWorkspaceTestGit(t, remote, "update-server-info")

	validationCalls := 0
	validateRoute := func(context.Context) error {
		validationCalls++
		if validationCalls > 1 {
			return db.ErrRepositoryRouteFenceChanged
		}
		return nil
	}
	ws := &Workspace{
		ID: "ws-base-route-fence", Platform: "github", PlatformHost: platformHost,
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeIssue,
		ItemNumber: 42, GitHeadRef: "issue-42",
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	_, err = mgr.addWorktree(
		t.Context(), workspaceGitDir{
			path: localRepo, remote: originRemoteName, localBase: true,
		}, ws, workspaceGitFetchOptions{
			validateRoute: validateRoute,
		},
	)

	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	restoredSHA, exists, refErr := gitRefSHA(
		t.Context(), localRepo, "refs/remotes/origin/main",
	)
	require.NoError(refErr)
	require.True(exists)
	require.Equal(originalSHA, restoredSHA)
	_, statErr := os.Stat(ws.WorktreePath)
	require.ErrorIs(statErr, os.ErrNotExist)
}

func TestAddWorktreeRestoresPullRefWhenRouteChangesDuringFetch(t *testing.T) {
	require := require.New(t)
	const prNumber = 43
	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/route-fence",
	)
	pullRef := fmt.Sprintf("refs/pull/%d/head", prNumber)
	runWorkspaceTestGit(
		t, remote, "update-ref", pullRef, "refs/heads/feature/route-fence",
	)
	runWorkspaceTestGit(t, remote, "update-server-info")
	_, exists, err := gitRefSHA(t.Context(), localRepo, pullRef)
	require.NoError(err)
	require.False(exists)

	validationCalls := 0
	validateRoute := func(context.Context) error {
		validationCalls++
		if validationCalls > 1 {
			return db.ErrRepositoryRouteFenceChanged
		}
		return nil
	}
	headRepo := "http://" + platformHost + "/acme/widget.git"
	ws := &Workspace{
		ID: "ws-pull-route-fence", Platform: "github", PlatformHost: platformHost,
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypePullRequest,
		ItemNumber: prNumber, GitHeadRef: "feature/route-fence",
		MRHeadRepo: &headRepo, WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	_, err = mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{
			path: localRepo, remote: originRemoteName, localBase: true,
		}, ws, workspaceGitFetchOptions{
			launchSpec:    pullLaunchSpecForWorkspace(ws, "same_repo", ""),
			validateRoute: validateRoute,
		},
	)

	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	_, exists, refErr := gitRefSHA(t.Context(), localRepo, pullRef)
	require.NoError(refErr)
	require.False(exists)
	_, statErr := os.Stat(ws.WorktreePath)
	require.ErrorIs(statErr, os.ErrNotExist)
}

func TestSyncLocalBaseBranchSkipsCheckedOutAndDivergedBranches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "main"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/base-sync")
	originalSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	)))
	firstRemoteSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "commit-tree", originalSHA+"^{tree}",
		"-p", originalSHA, "-m", "first remote base",
	)))
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/remotes/origin/main", firstRemoteSHA,
	)
	runWorkspaceTestGit(t, localRepo, "checkout", "--detach")

	require.NoError(syncLocalBaseBranch(
		t.Context(), localRepo, "origin", "ws-base-sync-safety", branch,
	))
	assert.Equal(firstRemoteSHA, strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	))))

	secondRemoteSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "commit-tree", firstRemoteSHA+"^{tree}",
		"-p", firstRemoteSHA, "-m", "second remote base",
	)))
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/remotes/origin/main", secondRemoteSHA,
	)
	runWorkspaceTestGit(t, localRepo, "checkout", branch)
	require.NoError(syncLocalBaseBranch(
		t.Context(), localRepo, "origin", "ws-base-sync-safety", branch,
	))
	assert.Equal(firstRemoteSHA, strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	))))

	runWorkspaceTestGit(t, localRepo, "checkout", "--detach")
	divergentSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "commit-tree", firstRemoteSHA+"^{tree}",
		"-p", firstRemoteSHA, "-m", "divergent local base",
	)))
	thirdRemoteSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "commit-tree", secondRemoteSHA+"^{tree}",
		"-p", secondRemoteSHA, "-m", "third remote base",
	)))
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/heads/main", divergentSHA,
	)
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/remotes/origin/main", thirdRemoteSHA,
	)
	require.NoError(syncLocalBaseBranch(
		t.Context(), localRepo, "origin", "ws-base-sync-safety", branch,
	))
	assert.Equal(divergentSHA, strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	))))
}

func TestSyncLocalBaseBranchSkipsOccupiedRefNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/base-sync")
	mainSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	)))
	runWorkspaceTestGit(t, localRepo, "checkout", "--detach")
	runWorkspaceTestGit(t, localRepo, "branch", "-D", "main")
	runWorkspaceTestGit(t, localRepo, "branch", "main/topic", mainSHA)

	require.NoError(syncLocalBaseBranch(
		t.Context(), localRepo, "origin", "ws-base-sync-namespace", "main",
	))
	_, mainExists, err := gitRefSHA(t.Context(), localRepo, "refs/heads/main")
	require.NoError(err)
	assert.False(mainExists)
	assert.Equal(mainSHA, strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main/topic",
	))))
}

func TestSyncWorkspaceBaseBranchRejectsReplacedRepositoryRoute(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/base-sync")
	localMainSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	)))
	remoteMainSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "commit-tree", localMainSHA+"^{tree}",
		"-p", localMainSHA, "-m", "replacement base",
	)))
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/remotes/origin/main", remoteMainSHA,
	)
	runWorkspaceTestGit(t, localRepo, "checkout", "--detach")

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	replacement, _, err := d.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform:       "github",
			PlatformHost:   "github.com",
			PlatformRepoID: "repo-acme-widget-replacement",
			Owner:          "acme",
			Name:           "widget",
		}, time.Now().UTC(),
	)
	require.NoError(err)
	require.NotNil(replacement)
	seedMR(t, d, replacement.Repository.ID, 968, "feature/base-sync")
	mgr := NewManager(d, t.TempDir())
	ws := &Workspace{
		ID:           "ws-base-sync-replaced-route",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   968,
	}

	err = mgr.syncWorkspaceBaseBranch(t.Context(), localRepo, originRemoteName, ws)
	require.ErrorContains(err, "historical occupants")
	assert.Equal(localMainSHA, strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/heads/main",
	))))
}

func TestFetchWorkspaceBaseConstrainsNegotiationTips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls [][]string
	run := func(_ context.Context, _ string, args ...string) error {
		calls = append(calls, slices.Clone(args))
		return nil
	}

	require.NoError(fetchWorkspaceBaseWithGit(
		t.Context(), run, t.TempDir(), originRemoteName, false,
	))
	require.NotEmpty(calls)
	fetchArgs := calls[0]
	assert.Contains(fetchArgs, "--negotiation-tip=refs/remotes/origin/*")
	assert.Contains(fetchArgs, "--recurse-submodules=no")
	assert.Contains(fetchArgs, "--no-tags")
}

func TestFetchWorkspaceBaseDisablesGitHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var calls [][]string
	run := func(_ context.Context, _ string, args ...string) error {
		calls = append(calls, slices.Clone(args))
		return nil
	}

	require.NoError(fetchWorkspaceBaseWithGit(
		t.Context(), run, t.TempDir(), originRemoteName, false,
	))
	require.Len(calls, 2)
	for _, args := range calls {
		require.GreaterOrEqual(len(args), 2)
		assert.Equal("-c", args[0])
		assert.Equal("core.hooksPath=/dev/null", args[1])
	}
	assert.Contains(calls[0], "fetch")
	assert.Contains(calls[1], "remote")
	assert.Contains(calls[1], "set-head")
}

func TestFetchWorkspaceBaseUsesResolvedRemoteNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls [][]string
	run := func(_ context.Context, _ string, args ...string) error {
		calls = append(calls, slices.Clone(args))
		return nil
	}

	require.NoError(fetchWorkspaceBaseWithGit(
		t.Context(), run, t.TempDir(), "upstream", true,
	))
	require.Len(calls, 2)
	assert.Contains(calls[0], "--negotiation-tip=refs/remotes/upstream/*")
	assert.Contains(calls[0], "upstream")
	assert.Contains(calls[0], "+refs/heads/*:refs/remotes/upstream/*")
	assert.Equal([]string{"remote", "set-head", "upstream", "-a"}, calls[1][2:])
}

func TestCleanupPreservesExistingWorktreeWhenConfiguredBaseChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-99"
	actualRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	wrongRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, actualRepo,
		"worktree", "add", worktreePath, "-b", branch, "HEAD",
	)
	runWorkspaceTestGit(t, wrongRepo, "branch", branch, "HEAD")

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(wrongRepo))
	ws := &Workspace{
		ID:              "ws-cleanup-existing-worktree",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))

	assert.DirExists(worktreePath)
	actualExists, err := localBranchExists(t.Context(), actualRepo, branch)
	require.NoError(err)
	assert.True(actualExists, "cleanup must preserve the unconfigured worktree")
	wrongExists, err := localBranchExists(t.Context(), wrongRepo, branch)
	require.NoError(err)
	assert.True(wrongExists, "cleanup must not delete branch from current settings repo")
}

func TestCleanupDeletesLiveRegisteredWorktreeWithoutOwnershipMarker(t *testing.T) {
	require := require.New(t)

	const branch = "kenn-forge/pr-42"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", worktreePath, "-b", branch, "HEAD",
	)

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	ws := &Workspace{
		ID:              "ws-unmarked-live-registration",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
	require.NoDirExists(worktreePath)
	branchExists, err := localBranchExists(t.Context(), localRepo, branch)
	require.NoError(err)
	require.False(branchExists)
}

func TestCleanupFindsManagedCloneAfterRepositoryRename(t *testing.T) {
	require := require.New(t)

	const branch = "kenn-forge/pr-42"
	oldClone := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, oldClone,
		"worktree", "add", worktreePath, "-b", branch, "HEAD",
	)

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID: "ws-renamed-clone", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "renamed",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
		GitHeadRef: "feature/thing", WorkspaceBranch: branch,
		WorktreePath: worktreePath,
	}
	require.NoError(writeWorkspaceOwnershipMarker(t.Context(), oldClone, ws))

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
	require.NoDirExists(worktreePath)
	branchExists, err := localBranchExists(t.Context(), oldClone, branch)
	require.NoError(err)
	require.False(branchExists)
}

func TestCleanupFindsMissingIdentityManagedWorktreeAfterRepositoryRename(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	observedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	identity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	}
	entry, _, err := database.ReconcileRepositoryObservation(
		t.Context(), identity, observedAt,
	)
	require.NoError(err)
	require.NotNil(entry)

	clones := gitclone.New(t.TempDir(), nil)
	cloneCtx := gitclone.WithRepositoryIdentity(
		t.Context(), identity.PlatformRepoID,
	)
	oldClone, err := clones.ClonePathForContext(
		cloneCtx, identity.Platform, identity.PlatformHost,
		identity.Owner, identity.Name,
	)
	require.NoError(err)
	seedWorkspaceBareCloneAt(t, oldClone)
	const branch = "kenn-forge/pr-42"
	worktreePath := filepath.Join(t.TempDir(), "missing-worktree")
	runWorkspaceTestGit(
		t, oldClone, "worktree", "add", worktreePath, "-b", branch, "HEAD",
	)
	ws := &Workspace{
		ID: "ws-renamed-missing-worktree", RepoID: entry.Repository.ID,
		Platform: identity.Platform, PlatformHost: identity.PlatformHost,
		RepoOwner: identity.Owner, RepoName: identity.Name,
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
		GitHeadRef: "feature/thing", WorkspaceBranch: branch,
		WorktreePath: worktreePath,
	}
	require.NoError(writeWorkspaceOwnershipMarker(t.Context(), oldClone, ws))
	require.NoError(os.RemoveAll(worktreePath))

	identity.Name = "renamed"
	_, _, err = database.ReconcileRepositoryObservation(
		t.Context(), identity, observedAt.Add(time.Minute),
	)
	require.NoError(err)
	ws.RepoName = identity.Name
	manager := newTestManager(t, database, t.TempDir())
	manager.SetClones(clones)

	require.NoError(manager.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
	branchExists, err := localBranchExists(t.Context(), oldClone, branch)
	require.NoError(err)
	require.False(branchExists)
	tracked, err := gitDirTracksWorktreePath(t.Context(), oldClone, worktreePath)
	require.NoError(err)
	require.False(tracked)
}

func TestCleanupDoesNotTrustReplacementCloneAtWorkspacePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-42"
	_, remote := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(t, "feature/thing")
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(t, t.TempDir(), "clone", remote, worktreePath)
	runWorkspaceTestGit(
		t, worktreePath, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(t, worktreePath, "branch", branch, "HEAD")

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:              "ws-replaced-clone",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	gitDir, ok, err := mgr.workspaceCleanupGitDir(t.Context(), ws)

	require.NoError(err)
	assert.False(ok)
	assert.Empty(gitDir)
	branchExists, err := localBranchExists(t.Context(), worktreePath, branch)
	require.NoError(err)
	assert.True(branchExists)
}

func TestCleanupDoesNotTrustStaleLocalBaseRegistrationForReplacementClone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-42"
	localRepo, remote := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(
		t, "feature/thing",
	)
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, localRepo,
		"worktree", "add", worktreePath, "-b", branch, "HEAD",
	)
	require.NoError(os.RemoveAll(worktreePath))
	runWorkspaceTestGit(t, t.TempDir(), "clone", remote, worktreePath)
	runWorkspaceTestGit(
		t, worktreePath, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(t, worktreePath, "branch", branch, "HEAD")

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
	ws := &Workspace{
		ID:              "ws-stale-local-base-replaced-clone",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	gitDir, ok, err := mgr.workspaceCleanupGitDir(t.Context(), ws)

	require.NoError(err)
	assert.False(ok)
	assert.Empty(gitDir)
	branchExists, err := localBranchExists(t.Context(), worktreePath, branch)
	require.NoError(err)
	assert.True(branchExists)
	_, err = os.Stat(worktreePath)
	require.NoError(err)
}

func TestCleanupIgnoresInvalidConfiguredBaseWhenWorktreeAbsent(t *testing.T) {
	require := require.New(t)

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(filepath.Join(t.TempDir(), "missing")))
	ws := &Workspace{
		ID:              "ws-cleanup-invalid-base",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: "kenn-forge/pr-99",
		WorktreePath:    filepath.Join(t.TempDir(), "already-removed"),
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
}

func TestCleanupSucceedsWhenWorkspacePathReplacedByNonGitDirectory(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, worktreePath string)
	}{
		{
			name: "plain directory",
			corrupt: func(t *testing.T, worktreePath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(
					filepath.Join(worktreePath, "leftover.txt"),
					[]byte("not a repo"), 0o644,
				))
			},
		},
		{
			name: "stale .git file from a removed repo",
			corrupt: func(t *testing.T, worktreePath string) {
				t.Helper()
				gone := filepath.Join(t.TempDir(), "gone", ".git", "worktrees", "x")
				require.NoError(t, os.WriteFile(
					filepath.Join(worktreePath, ".git"),
					[]byte("gitdir: "+gone+"\n"), 0o644,
				))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
			worktreePath := filepath.Join(t.TempDir(), "workspace")
			require.NoError(os.MkdirAll(worktreePath, 0o755))
			tt.corrupt(t, worktreePath)

			mgr := newTestManager(t, openTestDB(t), t.TempDir())
			mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))
			ws := &Workspace{
				ID:              "ws-cleanup-non-git-dir",
				Platform:        "github",
				PlatformHost:    "github.com",
				RepoOwner:       "acme",
				RepoName:        "widget",
				ItemType:        db.WorkspaceItemTypePullRequest,
				ItemNumber:      99,
				GitHeadRef:      "feature/thing",
				WorkspaceBranch: "kenn-forge/pr-99",
				WorktreePath:    worktreePath,
			}

			require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
			_, err := os.Lstat(worktreePath)
			require.ErrorIs(err, os.ErrNotExist)
			recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
			require.NoError(err)
			require.Len(recoveryPaths, 1)
			if tt.name == "plain directory" {
				contents, err := os.ReadFile(filepath.Join(recoveryPaths[0], "leftover.txt"))
				require.NoError(err)
				assert.Equal("not a repo", string(contents))
			} else {
				contents, err := os.ReadFile(filepath.Join(recoveryPaths[0], ".git"))
				require.NoError(err)
				assert.Contains(string(contents), "gitdir:")
			}
		})
	}
}

func TestQuarantineOrphanedWorkspacePathPreservesSymlinkToFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parent := t.TempDir()
	targetPath := filepath.Join(parent, "replacement-target")
	worktreePath := filepath.Join(parent, "workspace")
	require.NoError(os.WriteFile(
		targetPath, []byte("preserve target\n"), 0o644,
	))
	require.NoError(os.Symlink(targetPath, worktreePath))

	require.NoError(quarantineOrphanedWorkspacePath(t.Context(), worktreePath))
	_, err := os.Lstat(worktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
	require.NoError(err)
	require.Len(recoveryPaths, 1)
	recoveryInfo, err := os.Lstat(recoveryPaths[0])
	require.NoError(err)
	assert.NotZero(recoveryInfo.Mode() & os.ModeSymlink)
	contents, err := os.ReadFile(targetPath)
	require.NoError(err)
	assert.Equal("preserve target\n", string(contents))
}

func TestQuarantineOrphanedWorkspacePathPreservesBareRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	worktreePath := filepath.Join(t.TempDir(), "workspace.git")
	runWorkspaceTestGit(t, filepath.Dir(worktreePath), "init", "--bare", worktreePath)

	require.NoError(quarantineOrphanedWorkspacePath(t.Context(), worktreePath))
	_, err := os.Lstat(worktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
	require.NoError(err)
	require.Len(recoveryPaths, 1)
	bare, err := gitIsBareRepository(t.Context(), recoveryPaths[0])
	require.NoError(err)
	assert.True(bare)
}

func TestRemoveStaleWorktreeRegistrationMetadataResolvesRelativeGitdir(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	runWorkspaceTestGit(t, cloneDir, "config", "worktree.useRelativePaths", "true")
	worktreeRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(err)
	worktreePath := filepath.Join(worktreeRoot, "workspace")
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "add", worktreePath, "-b", "topic/relative", "HEAD",
	)
	metadataRoot := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir,
		"rev-parse", "--path-format=absolute", "--git-path", "worktrees",
	)))
	entries, err := os.ReadDir(metadataRoot)
	require.NoError(err)
	require.Len(entries, 1)
	metadataDir := filepath.Join(metadataRoot, entries[0].Name())
	gitFile, err := os.ReadFile(filepath.Join(metadataDir, "gitdir"))
	require.NoError(err)
	if filepath.IsAbs(strings.TrimSpace(string(gitFile))) {
		t.Skip("installed Git does not support relative worktree metadata")
	}
	assert.False(filepath.IsAbs(strings.TrimSpace(string(gitFile))))
	require.NoError(os.RemoveAll(worktreePath))

	require.NoError(removeStaleWorktreeRegistrationMetadata(
		t.Context(), cloneDir, worktreePath,
	))
	_, err = os.Lstat(metadataDir)
	require.ErrorIs(err, os.ErrNotExist)
	tracked, err := gitDirTracksWorktreePath(t.Context(), cloneDir, worktreePath)
	require.NoError(err)
	assert.False(tracked)
}

func TestCleanupQuarantinesPlainWorkspaceNestedInRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	parentRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	worktreePath := filepath.Join(parentRepo, "managed", "workspace")
	runWorkspaceTestGit(
		t, parentRepo,
		"worktree", "add", worktreePath, "-b", "topic/stale-nested", "HEAD",
	)
	require.NoError(os.RemoveAll(worktreePath))
	require.NoError(os.MkdirAll(worktreePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(worktreePath, "leftover.txt"), []byte("preserve me\n"), 0o644,
	))

	mgr := newTestManager(t, openTestDB(t), filepath.Join(parentRepo, "managed"))
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(parentRepo))
	ws := &Workspace{
		ID:              "ws-cleanup-nested-plain-dir",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: "kenn-forge/pr-99",
		WorktreePath:    worktreePath,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))
	_, err := os.Lstat(worktreePath)
	require.ErrorIs(err, os.ErrNotExist)
	recoveryPaths, err := filepath.Glob(worktreePath + ".orphaned-*")
	require.NoError(err)
	require.Len(recoveryPaths, 1)
	contents, err := os.ReadFile(filepath.Join(recoveryPaths[0], "leftover.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))
}

func TestNextWorkspaceRecoveryPathAvoidsCollisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	worktreePath := filepath.Join(t.TempDir(), "workspace")
	now := time.Date(2026, time.July, 31, 17, 15, 30, 0, time.UTC)
	want := worktreePath + ".orphaned-20260731T171530Z"

	got, err := nextWorkspaceRecoveryPath(worktreePath, now)
	require.NoError(err)
	assert.Equal(want, got)
	require.NoError(os.WriteFile(want, []byte("occupied"), 0o600))

	got, err = nextWorkspaceRecoveryPath(worktreePath, now)
	require.NoError(err)
	assert.Equal(want+"-2", got)
}

func TestCleanupFallsBackToManagedCloneWhenConfiguredBaseInvalid(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-99"
	cloneBaseDir := t.TempDir()
	clones := gitclone.New(cloneBaseDir, nil)
	cloneDir, err := clones.ClonePath("github", "github.com", "acme", "widget")
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(cloneDir), 0o755))
	runWorkspaceTestGit(
		t, cloneBaseDir, "clone", "--bare",
		setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing"),
		cloneDir,
	)
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "add", worktreePath, "-b", branch, "HEAD",
	)
	require.NoError(os.RemoveAll(worktreePath))

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetClones(clones)
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(filepath.Join(t.TempDir(), "missing")))
	ws := &Workspace{
		ID:              "ws-cleanup-managed-fallback",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))

	exists, err := localBranchExists(t.Context(), cloneDir, branch)
	require.NoError(err)
	assert.False(exists)
}

func TestCleanupUsesProviderScopedManagedClone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-99"
	const host = "forge.example.com"
	cloneBaseDir := t.TempDir()
	clones := gitclone.New(cloneBaseDir, nil)
	cloneDir, err := clones.ClonePathForContext(
		t.Context(), "gitlab", host, "acme", "widget",
	)
	require.NoError(err)
	require.NoError(os.MkdirAll(filepath.Dir(cloneDir), 0o755))
	runWorkspaceTestGit(
		t, cloneBaseDir, "clone", "--bare",
		setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing"),
		cloneDir,
	)
	worktreePath := filepath.Join(t.TempDir(), "workspace")
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "add", worktreePath, "-b", branch, "HEAD",
	)
	require.NoError(os.RemoveAll(worktreePath))

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetClones(clones)
	ws := &Workspace{
		ID:              "ws-cleanup-provider-scoped-managed",
		Platform:        "gitlab",
		PlatformHost:    host,
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    worktreePath,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))

	exists, err := localBranchExists(t.Context(), cloneDir, branch)
	require.NoError(err)
	assert.False(exists)
}

func TestCleanupSkipsReplacedWorktreeFromWrongRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "kenn-forge/pr-99"
	unrelatedRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/thing")
	runWorkspaceTestGit(
		t, unrelatedRepo, "remote", "set-url", "origin",
		"https://github.com/evil/widget.git",
	)
	runWorkspaceTestGit(t, unrelatedRepo, "branch", branch, "HEAD")

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:              "ws-cleanup-replaced-worktree",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      99,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: branch,
		WorktreePath:    unrelatedRepo,
	}

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))

	exists, err := localBranchExists(t.Context(), unrelatedRepo, branch)
	require.NoError(err)
	assert.True(exists)
}

func TestFailSetupUsesSinglePersistenceBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	repoID := seedRepo(
		t, d, "github.com", "acme", "widget",
	)
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)
	ws, err := mgr.Create(
		t.Context(), "github", "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(ws)

	origTimeout := workspacePersistTimeout
	workspacePersistTimeout = 200 * time.Millisecond
	t.Cleanup(func() { workspacePersistTimeout = origTimeout })

	tx, err := d.WriteDB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })

	start := time.Now()
	err = mgr.failSetup(
		t.Context(),
		ws.ID, workspaceSetupStageClone,
		errors.New("forced persistence timeout"),
	)
	elapsed := time.Since(start)

	require.Error(err)
	assert.Contains(err.Error(), "forced persistence timeout")
	assert.Less(
		elapsed,
		workspacePersistTimeout+(workspacePersistTimeout/2),
	)
}

func TestFailSetupRespectsParentDeadline(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	wtDir := t.TempDir()

	repoID := seedRepo(
		t, d, "github.com", "acme", "widget",
	)
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, wtDir)
	ws, err := mgr.Create(
		t.Context(), "github", "github.com", "acme", "widget", 42,
	)
	require.NoError(err)
	require.NotNil(ws)

	origTimeout := workspacePersistTimeout
	workspacePersistTimeout = time.Second
	t.Cleanup(func() { workspacePersistTimeout = origTimeout })

	tx, err := d.WriteDB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })

	parent, cancel := context.WithTimeout(
		t.Context(), 100*time.Millisecond,
	)
	defer cancel()

	start := time.Now()
	err = mgr.failSetup(
		parent,
		ws.ID, workspaceSetupStageClone,
		errors.New("forced persistence timeout"),
	)
	elapsed := time.Since(start)

	require.Error(err)
	assert.Contains(err.Error(), "forced persistence timeout")
	assert.Less(elapsed, 300*time.Millisecond)
}

func TestAddPreferredWorktreeRejectsUnsafeBranchName(t *testing.T) {
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:           "ws-unsafe-branch",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "-unsafe",
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}

	_, err := mgr.addPreferredWorktree(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws,
	)
	require.Error(err)
	require.Contains(err.Error(), "invalid branch name")
}

func TestValidateLocalBranchNameIgnoresBrokenWorkingTreeCwd(t *testing.T) {
	require := require.New(t)
	if os.Getenv("KENN_FORGE_TEST_VALIDATE_BRANCH_CWD") == "1" {
		require.NoError(os.Chdir(os.Getenv("KENN_FORGE_TEST_BROKEN_CWD")))
		require.NoError(validateLocalBranchName(
			t.Context(), "", "kenn-forge/issue-23-federation-test",
		))
		return
	}

	brokenCwd := t.TempDir()
	require.NoError(os.WriteFile(
		filepath.Join(brokenCwd, ".git"),
		[]byte("gitdir: /definitely/not/a/git/worktree\n"),
		0o644,
	))

	cmd := procutil.Command(
		os.Args[0],
		"-test.run=^TestValidateLocalBranchNameIgnoresBrokenWorkingTreeCwd$",
	)
	cmd.Env = append(
		os.Environ(),
		"KENN_FORGE_TEST_VALIDATE_BRANCH_CWD=1",
		"KENN_FORGE_TEST_BROKEN_CWD="+brokenCwd,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(err, string(out))
}

func TestAddWorktreeUsesFallbackWhenLocalBasePreferredBranchCheckedOut(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/thing"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, branch)
	existingWorktree := filepath.Join(t.TempDir(), "existing")
	runWorkspaceTestGit(
		t, localRepo, "worktree", "add", existingWorktree,
		"-b", branch, "refs/remotes/origin/"+branch,
	)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:           "ws-local-base-fallback",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   branch,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}

	gotBranch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName, localBase: true}, ws, workspaceGitFetchOptions{},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(42), gotBranch)
	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	originSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/"+branch,
	)))
	assert.Equal(originSHA, headSHA)
}

func TestAddWorktreeFallbackBranchTracksPRHeadBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	// Managed clones always carry the remote-tracking refspec (gitclone
	// EnsureRefspecs); without it git cannot resolve `@{upstream}` from
	// the branch config this test asserts on.
	runWorkspaceTestGit(
		t, cloneDir, "config", "remote.origin.fetch",
		"+refs/heads/*:refs/remotes/origin/*",
	)
	prNumber := 44
	headBranch := "feature/tracked-fallback"
	headSHA := configureSameRepoPRRefs(t, cloneDir, headBranch, prNumber)

	// Point the local branch with the preferred name at a divergent
	// commit so addPreferredWorktree rejects it and the synthetic
	// fallback branch is used instead.
	treeOut, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main^{tree}")
	require.NoError(err)
	divergentSHA, err := gitOutput(
		t.Context(), cloneDir,
		"commit-tree", strings.TrimSpace(treeOut),
		"-p", headSHA, "-m", "divergent local branch",
	)
	require.NoError(err)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref",
		"refs/heads/"+headBranch, strings.TrimSpace(divergentSHA),
	)

	ws := &Workspace{
		ID:              "ws-fallback-tracks-head",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      prNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-fallback-tracks-head",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
	)

	require.NoError(err)
	require.Equal(syntheticPRWorktreeBranch(prNumber), branch)
	remote, err := gitConfigValue(
		t.Context(), ws.WorktreePath, "branch."+branch+".remote",
	)
	require.NoError(err, "fallback branch must track the PR head branch")
	assert.Equal("origin", remote)
	mergeRef, err := gitConfigValue(
		t.Context(), ws.WorktreePath, "branch."+branch+".merge",
	)
	require.NoError(err)
	assert.Equal("refs/heads/"+headBranch, mergeRef)
	div, ok, err := WorktreeDivergence(t.Context(), ws.WorktreePath)
	require.NoError(err)
	require.True(ok, "divergence probe must resolve @{upstream}")
	assert.Equal(0, div.Ahead)
	assert.Equal(0, div.Behind)
}

func TestAddWorktreeLockedRecordsOwnershipBeforeReturning(t *testing.T) {
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const (
		prNumber   = 45
		headBranch = "feature/owned-registration"
	)
	configureSameRepoPRRefs(t, cloneDir, headBranch, prNumber)
	ws := &Workspace{
		ID:              "ws-owned-registration",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      prNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	_, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
	)
	require.NoError(err)
	owned, err := workspaceRegistrationMatches(
		t.Context(), cloneDir, ws.WorktreePath, ws.ID,
	)
	require.NoError(err)
	require.True(owned, "the registration must be marked before addWorktreeLocked returns")
}

func TestOwnedWorktreeAddRollsBackWhenOwnershipMarkerFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	branch := syntheticPRWorktreeBranch(46)
	ws := &Workspace{
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   46,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	_, err := mgr.runOwnedGitWorktreeAddCreatingBranch(
		t.Context(), cloneDir, ws, branch, "main",
	)
	require.Error(err)
	require.ErrorIs(err, errWorkspaceOwnershipMarker)
	_, statErr := os.Lstat(ws.WorktreePath)
	require.ErrorIs(statErr, os.ErrNotExist)
	tracked, trackErr := gitDirTracksWorktreePath(
		t.Context(), cloneDir, ws.WorktreePath,
	)
	require.NoError(trackErr)
	assert.False(tracked)
	_, exists, refErr := gitRefSHA(
		t.Context(), cloneDir, "refs/heads/"+branch,
	)
	require.NoError(refErr)
	assert.False(exists)
}

// divergentCommitForWorkspaceGitTest creates a commit that is not the PR head so
// a branch pointing at it makes addPreferredWorktree reject the preferred name.
func divergentCommitForWorkspaceGitTest(
	t *testing.T, cloneDir, parentSHA string,
) string {
	t.Helper()
	treeOut, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main^{tree}")
	require.NoError(t, err)
	sha, err := gitOutput(
		t.Context(), cloneDir,
		"commit-tree", strings.TrimSpace(treeOut),
		"-p", parentSHA, "-m", "divergent local branch",
	)
	require.NoError(t, err)
	require.NotEqual(t, parentSHA, strings.TrimSpace(sha))
	return strings.TrimSpace(sha)
}

// A stale local branch with the PR head name plus a leftover synthetic branch
// used to leave workspace creation with no usable branch name at all, and the
// maintainer with a permanent error and nothing to retry into.
func TestAddWorktreeUniquifiesFallbackBranchWhenSyntheticNameTaken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	runWorkspaceTestGit(
		t, cloneDir, "config", "remote.origin.fetch",
		"+refs/heads/*:refs/remotes/origin/*",
	)
	const prNumber = 746
	const headBranch = "fix/credential-rate-accounting"
	headSHA := configureSameRepoPRRefs(t, cloneDir, headBranch, prNumber)

	divergentSHA := divergentCommitForWorkspaceGitTest(t, cloneDir, headSHA)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/heads/"+headBranch, divergentSHA,
	)
	takenBranch := syntheticPRWorktreeBranch(prNumber)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/heads/"+takenBranch, divergentSHA,
	)

	ws := &Workspace{
		ID:              "ws-uniquified-fallback",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      prNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-uniquified-fallback",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
	)

	require.NoError(err)
	assert.Equal(takenBranch+"-2", branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
	// Neither pre-existing branch may be moved or deleted to make room.
	for _, existing := range []string{headBranch, takenBranch} {
		sha, ok, err := gitRefSHA(t.Context(), cloneDir, "refs/heads/"+existing)
		require.NoError(err)
		require.True(ok, "pre-existing branch %q must survive", existing)
		assert.Equal(divergentSHA, sha)
	}
	remote, err := gitConfigValue(
		t.Context(), ws.WorktreePath, "branch."+branch+".remote",
	)
	require.NoError(err, "uniquified fallback branch must track the PR head branch")
	assert.Equal("origin", remote)
	mergeRef, err := gitConfigValue(
		t.Context(), ws.WorktreePath, "branch."+branch+".merge",
	)
	require.NoError(err)
	assert.Equal("refs/heads/"+headBranch, mergeRef)
}

// The guarantee behind the uniquified fallback: no branch-name state in the
// repository can stop a workspace from being created. With every derived name
// taken, the worktree is checked out detached rather than failing.
func TestAddWorktreeFallsBackToDetachedWorktreeWhenBranchNamesExhausted(
	t *testing.T,
) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const prNumber = 747
	const headBranch = "fix/exhausted-branch-names"
	headSHA := configureSameRepoPRRefs(t, cloneDir, headBranch, prNumber)
	divergentSHA := divergentCommitForWorkspaceGitTest(t, cloneDir, headSHA)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/heads/"+headBranch, divergentSHA,
	)

	base := syntheticPRWorktreeBranch(prNumber)
	var refUpdates strings.Builder
	fmt.Fprintf(&refUpdates, "create refs/heads/%s %s\n", base, divergentSHA)
	for i := 2; i < 1000; i++ {
		fmt.Fprintf(
			&refUpdates, "create refs/heads/%s-%d %s\n", base, i, divergentSHA,
		)
	}
	_, stderr, err := gitcmd.New().Run(
		t.Context(), cloneDir,
		strings.NewReader(refUpdates.String()), "update-ref", "--stdin",
	)
	require.NoError(err, string(stderr))

	ws := &Workspace{
		ID:              "ws-detached-last-resort",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      prNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-detached-last-resort",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
	)

	require.NoError(err)
	// No managed branch: cleanup and delete must not remove anyone else's.
	assert.Empty(branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
	current, err := worktreeCurrentBranch(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Empty(current, "last-resort worktree must be detached")
}

func TestAddWorktreeUnknownHeadRepoDoesNotTrackMatchingOriginBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const prNumber = 45
	const headBranch = "feature/unknown-head-repo"
	headSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "rev-parse", "main",
	)))
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		headSHA+":refs/heads/"+headBranch,
	)
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		fmt.Sprintf("%s:refs/pull/%d/head", headSHA, prNumber),
	)
	runWorkspaceTestGit(
		t, cloneDir, "fetch", "origin",
		"+refs/heads/"+headBranch+":refs/remotes/origin/"+headBranch,
	)

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMRWithHeadRepo(t, d, repoID, prNumber, headBranch, "")
	mgr := newTestManager(t, d, t.TempDir())
	ws, err := mgr.Create(
		t.Context(), "github", "github.com", "acme", "widget", prNumber,
	)
	require.NoError(err)

	_, err = mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "unknown", ""),
		},
	)
	require.Error(err)
	assert.ErrorContains(err, "head repository identity is unavailable")
}

func TestAddWorktreeLocalBaseFetchesPullRefWhenHeadBranchDeleted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/deleted"
	const prNumber = 43
	localRepo, remote, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, branch,
	)
	wantSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/"+branch,
	)))
	runWorkspaceTestGit(
		t, remote, "update-ref",
		fmt.Sprintf("refs/pull/%d/head", prNumber), wantSHA,
	)
	runWorkspaceTestGit(t, remote, "update-ref", "-d", "refs/heads/"+branch)
	runWorkspaceTestGit(t, remote, "update-server-info")
	runWorkspaceTestGit(t, localRepo, "fetch", "--prune", "origin")
	_, exists, err := gitRefSHA(
		t.Context(), localRepo, "refs/remotes/origin/"+branch,
	)
	require.NoError(err)
	require.False(exists)
	_, exists, err = gitRefSHA(
		t.Context(), localRepo,
		fmt.Sprintf("refs/pull/%d/head", prNumber),
	)
	require.NoError(err)
	require.False(exists)

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:           "ws-add-fetches-pull-ref",
		Platform:     "github",
		PlatformHost: platformHost,
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   prNumber,
		GitHeadRef:   branch,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}

	gotBranch, err := mgr.addWorktree(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName, localBase: true}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "same_repo", ""),
		},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(prNumber), gotBranch)
	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(wantSHA, headSHA)
}

func TestAddWorktreeLocalBaseIgnoresStalePullRefWhenFetchFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/live"
	const prNumber = 44
	localRepo, _, _ := setupHTTPWorktreeBaseForWorkspaceGitTest(t, branch)
	originSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/"+branch,
	)))
	treeSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "main^{tree}",
	)))
	staleSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo,
		"commit-tree", treeSHA,
		"-p", originSHA,
		"-m", "stale pull head",
	)))
	require.NotEqual(originSHA, staleSHA)
	runWorkspaceTestGit(
		t, localRepo, "update-ref",
		fmt.Sprintf("refs/pull/%d/head", prNumber), staleSHA,
	)
	existingWorktree := filepath.Join(t.TempDir(), "existing")
	runWorkspaceTestGit(
		t, localRepo, "worktree", "add", existingWorktree,
		"-b", branch, "refs/remotes/origin/"+branch,
	)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:           "ws-add-ignores-stale-pull-ref",
		Platform:     "github",
		PlatformHost: "127.0.0.1",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   prNumber,
		GitHeadRef:   branch,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}

	gotBranch, err := mgr.addWorktree(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName, localBase: true}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "same_repo", ""),
		},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(prNumber), gotBranch)
	headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(originSHA, headSHA)
}

func TestLocalBaseExistingPRBranchIsNotDeletedOnCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/thing"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, branch)
	runWorkspaceTestGit(t, localRepo, "branch", branch, "refs/remotes/origin/"+branch)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:              "ws-existing-local-pr-branch",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      branch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-existing-local-pr-branch",
		Status:          "ready",
	}

	managedBranch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName, localBase: true}, ws, workspaceGitFetchOptions{},
	)
	require.NoError(err)
	require.Empty(managedBranch)
	ws.WorkspaceBranch = managedBranch

	require.NoError(mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws))

	exists, err := localBranchExists(t.Context(), localRepo, branch)
	require.NoError(err)
	assert.True(exists)
}

func TestLocalBaseExistingPRBranchPreservesUpstream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "feature/thing"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, branch)
	runWorkspaceTestGit(t, localRepo, "branch", branch, "refs/remotes/origin/"+branch)
	runWorkspaceTestGit(t, localRepo, "config", "branch."+branch+".remote", "upstream")
	runWorkspaceTestGit(t, localRepo, "config", "branch."+branch+".merge", "refs/heads/main")
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:              "ws-existing-local-pr-branch-upstream",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      branch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-existing-local-pr-branch-upstream",
		Status:          "ready",
	}

	managedBranch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName, localBase: true}, ws, workspaceGitFetchOptions{},
	)

	require.NoError(err)
	assert.Empty(managedBranch)
	remote, err := gitConfigValue(t.Context(), localRepo, "branch."+branch+".remote")
	require.NoError(err)
	assert.Equal("upstream", remote)
	mergeRef, err := gitConfigValue(t.Context(), localRepo, "branch."+branch+".merge")
	require.NoError(err)
	assert.Equal("refs/heads/main", mergeRef)
}

func TestAddPreferredWorktreeHeadRepoRouting(t *testing.T) {
	type worktreeExpectation struct {
		headSHA  string
		remote   string
		mergeRef string
	}

	tests := []struct {
		name        string
		number      int
		headBranch  string
		headRepoURL string
		configure   func(*testing.T, string, string, int) worktreeExpectation
	}{
		{
			name:        "same-repo PR tracks real remote branch",
			number:      244,
			headBranch:  "feature/thing",
			headRepoURL: "https://github.com/acme/widget.git",
			configure: func(
				t *testing.T, cloneDir, branch string, prNumber int,
			) worktreeExpectation {
				// Reproduce the dangerous repo state from issue #256: the real
				// branch and GitHub's synthetic pull ref both exist and point at
				// the same commit. Starting from refs/pull/<number>/head lets Git
				// auto-configure that synthetic ref as the upstream, which breaks
				// tools that inspect @{u}.
				sha := configureSameRepoPRRefs(
					t, cloneDir, branch, prNumber,
				)
				return worktreeExpectation{
					headSHA:  sha,
					remote:   "origin",
					mergeRef: "refs/heads/" + branch,
				}
			},
		},
		{
			name:        "fork PR prefers pull ref over same-named origin branch",
			number:      245,
			headBranch:  "fork/thing",
			headRepoURL: "https://github.com/contributor/widget.git",
			configure: func(
				t *testing.T, cloneDir, branch string, prNumber int,
			) worktreeExpectation {
				// A base repo can have a branch with the same name as a fork PR
				// branch, but that origin branch is not the fork head. Fork
				// workspaces must prefer the GitHub pull ref over any same-named
				// origin branch.
				originSHA, pullSHA := configureForkPRRefs(
					t, cloneDir, branch, prNumber,
				)
				gotOriginSHA, exists, err := gitRefSHA(
					t.Context(), cloneDir, "refs/remotes/origin/"+branch,
				)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotEqual(t, originSHA, pullSHA)
				require.Equal(t, originSHA, gotOriginSHA)
				return worktreeExpectation{headSHA: pullSHA}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cloneDir := setupBareCloneForWorkspaceGitTest(t)
			want := tt.configure(t, cloneDir, tt.headBranch, tt.number)

			d := openTestDB(t)
			repoID := seedRepo(t, d, "github.com", "acme", "widget")
			seedMRWithHeadRepo(
				t, d, repoID, tt.number, tt.headBranch, tt.headRepoURL,
			)
			mgr := newTestManager(t, d, t.TempDir())
			ws, err := mgr.Create(
				t.Context(), "github", "github.com", "acme", "widget", tt.number,
			)
			require.NoError(err)

			branch, err := mgr.addPreferredWorktree(t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws)
			require.NoError(err)
			assert.Equal(tt.headBranch, branch)

			headSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
			require.NoError(err)
			assert.Equal(want.headSHA, headSHA)

			if want.remote == "" && want.mergeRef == "" {
				return
			}
			remote, err := gitConfigValue(
				t.Context(), ws.WorktreePath,
				"branch."+tt.headBranch+".remote",
			)
			require.NoError(err)
			mergeRef, err := gitConfigValue(
				t.Context(), ws.WorktreePath,
				"branch."+tt.headBranch+".merge",
			)
			require.NoError(err)
			assert.Equal(want.remote, remote)
			assert.Equal(want.mergeRef, mergeRef)
		})
	}
}

func TestAddWorktreeGitLabForkMRFetchesHeadBeforePreferredBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	mrNumber := 547
	headBranch := "contributor/gitlab-fork"
	treeSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "rev-parse", "main^{tree}",
	)))
	headSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "commit-tree", treeSHA, "-p", "main",
		"-m", "remote fork mr head",
	)))
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		fmt.Sprintf("%s:refs/merge-requests/%d/head", headSHA, mrNumber),
	)
	forkRemote := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "remote", "get-url", "origin",
	)))
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin", headSHA+":refs/heads/"+headBranch,
	)
	headRepo := forkRemote
	ws := &Workspace{
		ID:              "ws-gitlab-fork-mr-preferred",
		Platform:        "gitlab",
		PlatformHost:    "gitlab.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      mrNumber,
		GitHeadRef:      headBranch,
		MRHeadRepo:      &headRepo,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-gitlab-fork-mr-preferred",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "fork", forkRemote),
		},
	)

	require.NoError(err)
	assert.Equal(headBranch, branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
}

func TestAddWorktreeMergedSameRepoPRUsesPullRefWhenHeadBranchDeleted(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	prNumber := 545
	headBranch := "codex/wildcard-promote-local-clones"
	headSHA := configureSameRepoPRRefs(t, cloneDir, headBranch, prNumber)
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		fmt.Sprintf("%s:refs/pull/%d/head", headSHA, prNumber),
	)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "-d",
		"refs/remotes/origin/"+headBranch,
	)

	ws := &Workspace{
		ID:              "ws-merged-same-repo-pr",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "kenn-forge",
		RepoName:        "kenn-forge",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      prNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-merged-same-repo-pr",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "same_repo", ""),
		},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(prNumber), branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
	// The head branch no longer exists on origin, so there is nothing
	// for the fallback branch to track.
	_, err = gitConfigValue(
		t.Context(), ws.WorktreePath, "branch."+branch+".remote",
	)
	assert.Error(err, "deleted head branch must not be configured as upstream")
}

func TestAddWorktreeGitLabMRUsesMergeRequestRefWhenHeadBranchDeleted(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	mrNumber := 546
	headBranch := "feature/gitlab-merged"
	headSHA := configureGitLabMRRefs(t, cloneDir, headBranch, mrNumber)
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		fmt.Sprintf("%s:refs/merge-requests/%d/head", headSHA, mrNumber),
	)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "-d",
		"refs/remotes/origin/"+headBranch,
	)

	ws := &Workspace{
		ID:              "ws-merged-gitlab-mr",
		Platform:        "gitlab",
		PlatformHost:    "gitlab.com",
		RepoOwner:       "kenn-forge",
		RepoName:        "kenn-forge",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      mrNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-merged-gitlab-mr",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "same_repo", ""),
		},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(mrNumber), branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
}

func TestAddWorktreeGitLabMRFetchesSpecificMergeRequestRef(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	mrNumber := 546
	headBranch := "feature/gitlab-merged"
	treeSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "rev-parse", "main^{tree}",
	)))
	headSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "commit-tree", treeSHA, "-p", "main",
		"-m", "remote mr head",
	)))
	runWorkspaceTestGit(
		t, cloneDir, "push", "origin",
		fmt.Sprintf("%s:refs/merge-requests/%d/head", headSHA, mrNumber),
	)

	ws := &Workspace{
		ID:              "ws-merged-gitlab-mr-specific-fetch",
		Platform:        "gitlab",
		PlatformHost:    "gitlab.com",
		RepoOwner:       "kenn-forge",
		RepoName:        "kenn-forge",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      mrNumber,
		GitHeadRef:      headBranch,
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "ws-merged-gitlab-mr-specific-fetch",
		Status:          "creating",
	}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	branch, err := mgr.addWorktreeLocked(
		t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{
			launchSpec: pullLaunchSpecForWorkspace(ws, "same_repo", ""),
		},
	)

	require.NoError(err)
	assert.Equal(syntheticPRWorktreeBranch(mrNumber), branch)
	gotSHA, err := gitHeadSHA(t.Context(), ws.WorktreePath)
	require.NoError(err)
	assert.Equal(headSHA, gotSHA)
	gotRef := strings.TrimSpace(string(runWorkspaceTestGit(
		t, cloneDir, "rev-parse",
		fmt.Sprintf("refs/merge-requests/%d/head", mrNumber),
	)))
	assert.Equal(headSHA, gotRef)
}

func TestRollbackWorktreeDeletesBranchWhenContextCanceled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	branch := syntheticPRWorktreeBranch(42)
	ws := &Workspace{
		ID:           "ws-canceled-rollback",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}
	runWorkspaceTestGit(
		t, cloneDir,
		"worktree", "add", ws.WorktreePath, "-b", branch, "main",
	)
	require.NoError(writeWorkspaceOwnershipMarker(t.Context(), cloneDir, ws))
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	mgr.rollbackWorktree(ctx, cloneDir, ws, workspaceBranchUnknown)

	_, exists, err := gitRefSHA(
		t.Context(), cloneDir, "refs/heads/"+branch,
	)
	require.NoError(err)
	assert.False(exists)
}

func TestRollbackWorktreePreservesReplacementWithoutOwnershipMarker(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	branch := syntheticPRWorktreeBranch(43)
	ws := &Workspace{
		ID:           "ws-replaced-before-rollback",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   43,
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
	}
	runWorkspaceTestGit(
		t, cloneDir,
		"worktree", "add", ws.WorktreePath, "-b", branch, "main",
	)
	require.NoError(writeWorkspaceOwnershipMarker(t.Context(), cloneDir, ws))
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "remove", "--force", ws.WorktreePath,
	)
	runWorkspaceTestGit(t, cloneDir, "worktree", "add", ws.WorktreePath, branch)
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "replacement.txt"),
		[]byte("uncommitted replacement\n"), 0o644,
	))

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.rollbackWorktree(t.Context(), cloneDir, ws, branch)

	contents, err := os.ReadFile(filepath.Join(ws.WorktreePath, "replacement.txt"))
	require.NoError(err)
	assert.Equal("uncommitted replacement\n", string(contents))
	branchSHA, exists, err := gitRefSHA(
		t.Context(), cloneDir, "refs/heads/"+branch,
	)
	require.NoError(err)
	assert.True(exists)
	assert.NotEmpty(branchSHA)
}

func TestLocalBranchExistsIgnoresInheritedGitEnv(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	targetClone := setupBareCloneForWorkspaceGitTest(t)
	poisonClone := setupBareCloneForWorkspaceGitTest(t)
	require.NoError(runGitWithoutHooks(
		context.Background(), poisonClone,
		"branch", "kenn-forge/issue-7", "main",
	))

	t.Setenv("GIT_DIR", poisonClone)
	t.Setenv("GIT_WORK_TREE", t.TempDir())

	exists, err := localBranchExists(
		context.Background(), targetClone, "kenn-forge/issue-7",
	)

	require.NoError(err)
	assert.False(exists)
}

func TestCleanupContextRespectsParentDeadline(t *testing.T) {
	require := require.New(t)

	parent, cancel := context.WithTimeout(
		t.Context(), 100*time.Millisecond,
	)
	defer cancel()

	cleanupCtx, cleanupCancel := cleanupContext(parent)
	defer cleanupCancel()

	deadline, ok := cleanupCtx.Deadline()
	require.True(ok)

	remaining := time.Until(deadline)
	require.LessOrEqual(remaining, 100*time.Millisecond)
	require.Greater(remaining, 0*time.Millisecond)
}

func setupBareCloneForWorkspaceGitTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	cloneDir := filepath.Join(dir, "clone.git")

	runWorkspaceTestGit(
		t, dir, "init", "--bare", "--initial-branch=main", remote,
	)
	runWorkspaceTestGit(t, dir, "clone", remote, work)
	runWorkspaceTestGit(
		t, work, "config", "user.email", "test@test.com",
	)
	runWorkspaceTestGit(
		t, work, "config", "user.name", "Test",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(work, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, work, "push", "origin", "main")
	runWorkspaceTestGit(t, dir, "clone", "--bare", remote, cloneDir)
	runWorkspaceTestGit(t, cloneDir, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, cloneDir, "config", "user.name", "Test")

	return cloneDir
}

func seedWorkspaceBareCloneAt(t *testing.T, cloneDir string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")

	runWorkspaceTestGit(
		t, dir, "init", "--bare", "--initial-branch=main", remote,
	)
	runWorkspaceTestGit(t, dir, "clone", remote, work)
	runWorkspaceTestGit(
		t, work, "config", "user.email", "test@test.com",
	)
	runWorkspaceTestGit(
		t, work, "config", "user.name", "Test",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(work, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, work, "push", "origin", "main")
	require.NoError(t, os.MkdirAll(filepath.Dir(cloneDir), 0o755))
	runWorkspaceTestGit(t, dir, "clone", "--bare", remote, cloneDir)
	runWorkspaceTestGit(t, cloneDir, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, cloneDir, "config", "user.name", "Test")
}

func configureOriginHeadForIssueWorkspace(t *testing.T, cloneDir string) {
	t.Helper()
	out, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main")
	require.NoError(t, err)
	sha := strings.TrimSpace(out)
	require.NotEmpty(t, sha)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/remotes/origin/main", sha,
	)
	runWorkspaceTestGit(
		t, cloneDir, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main",
	)
}

func setupLocalWorktreeBaseForWorkspaceGitTest(
	t *testing.T, branch string,
) string {
	t.Helper()
	repo, _ := setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(t, branch)
	return repo
}

func setupLocalWorktreeBaseWithRemoteForWorkspaceGitTest(
	t *testing.T, branch string,
) (string, string) {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	repo := filepath.Join(dir, "repo")
	runWorkspaceTestGit(
		t, dir, "init", "--bare", "--initial-branch=main", remote,
	)
	runWorkspaceTestGit(t, dir, "init", "--initial-branch=main", repo)
	runWorkspaceTestGit(t, repo, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, repo, "config", "user.name", "Test")
	runWorkspaceTestGit(
		t, repo, "remote", "add", "origin",
		remote,
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, repo, "add", ".")
	runWorkspaceTestGit(t, repo, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, repo, "push", "origin", "HEAD:refs/heads/main")
	runWorkspaceTestGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runWorkspaceTestGit(t, repo, "push", "origin", "HEAD:refs/heads/"+branch)
	runWorkspaceTestGit(
		t, repo, "remote", "set-url", "origin",
		"https://github.com/acme/widget.git",
	)
	runWorkspaceTestGit(
		t, repo, "update-ref", "refs/remotes/origin/"+branch, "HEAD",
	)
	runWorkspaceTestGit(
		t, repo, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main",
	)
	return repo, remote
}

func setupHTTPWorktreeBaseForWorkspaceGitTest(
	t *testing.T, branch string,
) (repo, remote, platformHost string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "acme", "widget.git")
	repo = filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(filepath.Dir(remote), 0o755))
	runWorkspaceTestGit(
		t, root, "init", "--bare", "--initial-branch=main", remote,
	)
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)
	remoteURL := server.URL + "/acme/widget.git"
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	platformHost = parsed.Host

	runWorkspaceTestGit(t, root, "init", "--initial-branch=main", repo)
	runWorkspaceTestGit(t, repo, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, repo, "config", "user.name", "Test")
	runWorkspaceTestGit(t, repo, "remote", "add", "origin", remote)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, repo, "add", ".")
	runWorkspaceTestGit(t, repo, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, repo, "push", "origin", "HEAD:refs/heads/main")
	runWorkspaceTestGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runWorkspaceTestGit(t, repo, "push", "origin", "HEAD:refs/heads/"+branch)
	runWorkspaceTestGit(t, remote, "update-server-info")
	runWorkspaceTestGit(t, repo, "remote", "set-url", "origin", remoteURL)
	runWorkspaceTestGit(t, repo, "fetch", "--prune", "origin")
	runWorkspaceTestGit(
		t, repo, "symbolic-ref",
		"refs/remotes/origin/HEAD", "refs/remotes/origin/main",
	)
	return repo, remote, platformHost
}

func setupForkStyleHTTPWorktreeBaseForWorkspaceGitTest(
	t *testing.T, branch string,
) (repo, remote, platformHost string) {
	t.Helper()
	repo, remote, platformHost = setupHTTPWorktreeBaseForWorkspaceGitTest(t, branch)
	runWorkspaceTestGit(t, repo, "remote", "rename", "origin", "upstream")
	runWorkspaceTestGit(
		t, repo, "remote", "add", "origin",
		"https://"+platformHost+"/forker/widget.git",
	)
	return repo, remote, platformHost
}

func setupRouteChangingCloneRemoteForWorkspaceTest(
	t *testing.T, onFirstRequest func() error,
) (platformHost, cloneURL string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "acme", "widget.git")
	work := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(filepath.Dir(remote), 0o755))
	runWorkspaceTestGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	runWorkspaceTestGit(t, root, "init", "--initial-branch=main", work)
	runWorkspaceTestGit(t, work, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, work, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(
		filepath.Join(work, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, work, "remote", "add", "origin", remote)
	runWorkspaceTestGit(t, work, "push", "origin", "main")
	runWorkspaceTestGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runWorkspaceTestGit(t, remote, "update-server-info")

	files := http.FileServer(http.Dir(root))
	var once sync.Once
	var routeChangeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { routeChangeErr = onFirstRequest() })
		if routeChangeErr != nil {
			http.Error(w, routeChangeErr.Error(), http.StatusInternalServerError)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		require.NoError(t, routeChangeErr)
	})
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	return parsed.Host, server.URL + "/acme/widget.git"
}

func setupRemoteForForkPRWorktreeTest(
	t *testing.T, branch string, prNumber int,
) (remote, pullSHA string) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	remote = filepath.Join(dir, "remote.git")
	runWorkspaceTestGit(t, dir, "init", "--initial-branch=main", work)
	runWorkspaceTestGit(t, work, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, work, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(
		filepath.Join(work, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "base commit")
	runWorkspaceTestGit(t, dir, "init", "--bare", "--initial-branch=main", remote)
	runWorkspaceTestGit(t, work, "remote", "add", "origin", remote)
	runWorkspaceTestGit(t, work, "push", "origin", "main")

	originSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, work, "rev-parse", "HEAD",
	)))
	require.NoError(t, os.WriteFile(
		filepath.Join(work, "fork.txt"), []byte("fork\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "fork head")
	pullSHA = strings.TrimSpace(string(runWorkspaceTestGit(
		t, work, "rev-parse", "HEAD",
	)))
	require.NotEqual(t, originSHA, pullSHA)
	runWorkspaceTestGit(
		t, work, "push", "origin",
		pullSHA+":refs/heads/"+branch,
		pullSHA+":refs/pull/"+strconv.Itoa(prNumber)+"/head",
	)
	return remote, pullSHA
}

func configureSameRepoPRRefs(
	t *testing.T, cloneDir, branch string, prNumber int,
) string {
	t.Helper()
	out, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main")
	require.NoError(t, err)
	sha := strings.TrimSpace(out)
	require.NotEmpty(t, sha)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/remotes/origin/"+branch, sha,
	)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref",
		fmt.Sprintf("refs/pull/%d/head", prNumber), sha,
	)
	return sha
}

func configureGitLabMRRefs(
	t *testing.T, cloneDir, branch string, mrNumber int,
) string {
	t.Helper()
	out, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main")
	require.NoError(t, err)
	sha := strings.TrimSpace(out)
	require.NotEmpty(t, sha)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/remotes/origin/"+branch, sha,
	)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref",
		fmt.Sprintf("refs/merge-requests/%d/head", mrNumber), sha,
	)
	return sha
}

func configureForkPRRefs(
	t *testing.T, cloneDir, branch string, prNumber int,
) (originSHA, pullSHA string) {
	t.Helper()
	out, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main")
	require.NoError(t, err)
	originSHA = strings.TrimSpace(out)
	require.NotEmpty(t, originSHA)
	treeOut, err := gitOutput(t.Context(), cloneDir, "rev-parse", "main^{tree}")
	require.NoError(t, err)
	runWorkspaceTestGit(t, cloneDir, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, cloneDir, "config", "user.name", "Test")
	commitOut, err := gitOutput(
		t.Context(), cloneDir,
		"commit-tree", strings.TrimSpace(treeOut),
		"-p", originSHA, "-m", "fork head",
	)
	require.NoError(t, err)
	pullSHA = strings.TrimSpace(commitOut)
	require.NotEmpty(t, pullSHA)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref", "refs/remotes/origin/"+branch, originSHA,
	)
	runWorkspaceTestGit(
		t, cloneDir, "update-ref",
		fmt.Sprintf("refs/pull/%d/head", prNumber), pullSHA,
	)
	return originSHA, pullSHA
}

func runWorkspaceTestGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
	return out
}

func TestShellFromPasswdLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			"normal zsh",
			"wesm:x:501:20:Wes McKinney:/Users/wesm:/bin/zsh",
			"/bin/zsh",
		},
		{
			"normal bash",
			"dev:x:1000:1000::/home/dev:/bin/bash",
			"/bin/bash",
		},
		{
			"nologin filtered",
			"nobody:x:65534:65534:Nobody:/nonexistent:/sbin/nologin",
			"",
		},
		{
			"false filtered",
			"git:x:998:998::/home/git:/usr/bin/false",
			"",
		},
		{
			"bin/false filtered",
			"svc:x:999:999::/srv:/bin/false",
			"",
		},
		{
			"empty shell",
			"user:x:1000:1000::/home/user:",
			"",
		},
		{
			"too few fields",
			"broken:line",
			"",
		},
		{
			"empty line",
			"",
			"",
		},
		{
			"trailing whitespace",
			"user:x:1000:1000::/home/user:/bin/zsh\n",
			"/bin/zsh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellFromPasswdLine(tt.line)
			require.Equal(t, tt.want, got)
		})
	}
}

// writeRecorderScript creates an executable shell script at a
// fresh path under t.TempDir() that appends the count and each
// argument, NUL-delimited, to TMUX_RECORD. Returns the script path
// and the record file path.
func writeRecorderScript(t *testing.T) (scriptPath, recordPath string) {
	t.Helper()
	dir := t.TempDir()
	recordPath = filepath.Join(dir, "record")
	scriptPath = filepath.Join(dir, "fake-tmux")
	// The record path is baked into the script: tmux clients run with
	// the non-secret allowlist environment, so fixtures cannot smuggle
	// paths through custom env vars.
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(recordPath) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(body), 0o755))
	return scriptPath, recordPath
}

// readRecorderArgv reads the NUL-delimited record file and returns
// each recorded invocation as a []string. Each invocation is stored
// as "<argc>\0<arg0>\0<arg1>...\0", so this reads argc then slurps
// that many args per invocation.
func readRecorderArgv(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// Split on NUL. Each record is "<argc>\0<arg0>\0<arg1>\0...\0",
	// so the flushed stream always ends with a trailing \0 and Split
	// produces a final empty element after it. Strip exactly one
	// trailing empty so we don't mistake it for part of the next
	// record. Interior empty elements are real args (the NUL framing
	// exists to preserve them) and must NOT be skipped.
	parts := strings.Split(string(data), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	var out [][]string
	for i := 0; i < len(parts); {
		n, err := strconv.Atoi(parts[i])
		require.NoError(t, err)
		i++
		argv := parts[i : i+n]
		for j := range argv {
			argv[j] = normalizeRecordedTmuxArg(argv[j])
		}
		out = append(out, argv)
		i += n
	}
	return out
}

func normalizeRecordedTmuxArg(arg string) string {
	if runtime.GOOS != "windows" {
		return arg
	}
	switch arg {
	case "#session_name":
		return "#{session_name}"
	case "#pane_title":
		return "#{pane_title}"
	default:
		return arg
	}
}

func TestManagerEnsureTmuxHasSessionPrefix(t *testing.T) {
	assert := assert.New(t)

	script, record := writeRecorderScript(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetTmuxGraphics(true)
	mgr.SetTmuxMouse(true)

	// Script exits 0 for every invocation, so EnsureTmux observes
	// "session exists" after the has-session call and refreshes the
	// Forge-owned terminal options without running new-session.
	require.NoError(t, mgr.EnsureTmux(t.Context(), "sess-A", t.TempDir()))

	argvs := readRecorderArgv(t, record)
	require.Len(t, argvs, 2)
	assert.Equal(
		[]string{"wrap", "has-session", "-t", "sess-A"},
		argvs[0],
	)
	assert.Equal(
		[]string{
			"wrap", "set-option", "-q", "-p", "-t", "sess-A",
			"allow-passthrough", "on",
		},
		argvs[1],
	)
}

func TestManagerApplyTmuxMouseUpdatesDedicatedServer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`case "$*" in *list-sessions*) printf 'sess-A:kenn-forge:test-owner\n';; esac` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxMouse(true)
	require.NoError(mgr.ApplyTmuxMouse(t.Context()))

	assert.Equal([][]string{
		{"-L", "kenn-forge", "list-sessions", "-F", tmuxSessionListFormat},
		{"-L", "kenn-forge", "set-option", "-q", "-g", "mouse", "on"},
	}, readRecorderArgv(t, record))
}

func TestManagerApplyTmuxGraphicsDisablesDedicatedServer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`case "$*" in *list-sessions*) printf 'sess-A:kenn-forge:test-owner\n';; *list-panes*) printf 'pane-A\npane-B\n';; esac` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxGraphics(false)
	require.NoError(mgr.ApplyTmuxGraphics(t.Context()))

	assert.Equal([][]string{
		{"-L", "kenn-forge", "list-sessions", "-F", tmuxSessionListFormat},
		{"-L", "kenn-forge", "set-option", "-q", "-g", "allow-passthrough", "off"},
		{"-L", "kenn-forge", "set-option", "-q", "-s", "-u", "terminal-features[100]"},
		{"-L", "kenn-forge", "list-panes", "-s", "-t", "sess-A", "-F", tmuxPaneListFormat},
		{"-L", "kenn-forge", "set-option", "-q", "-p", "-u", "-t", "pane-A", "allow-passthrough"},
		{"-L", "kenn-forge", "set-option", "-q", "-p", "-u", "-t", "pane-B", "allow-passthrough"},
	}, readRecorderArgv(t, record))
}

func TestManagerApplyTmuxGraphicsEnablesEveryOwnedCustomPane(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux")
	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxPath})
	mgr.SetTmuxGraphics(true)
	body := fmt.Sprintf("#!/bin/sh\n"+
		"TMUX_RECORD=%s\n"+
		`printf '%%s\0' "$#" "$@" >> "$TMUX_RECORD"`+"\n"+
		`case "$*" in *list-sessions*) printf 'owned:%s\nother:user-owned\n';; *list-panes*) printf 'pane-A\npane-B\n';; esac`+"\n"+
		"exit 0\n", shellquote.Join(record), mgr.TmuxOwnerMarker())
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))

	require.NoError(mgr.ApplyTmuxGraphics(t.Context()))

	assert.Equal([][]string{
		{"list-sessions", "-F", tmuxSessionListFormat},
		{"list-panes", "-s", "-t", "owned", "-F", tmuxPaneListFormat},
		{"set-option", "-q", "-p", "-t", "pane-A", "allow-passthrough", "on"},
		{"set-option", "-q", "-p", "-t", "pane-B", "allow-passthrough", "on"},
	}, readRecorderArgv(t, record))
}

func TestManagerApplyTmuxGraphicsAttemptsEveryPaneAfterFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	tmuxPath := filepath.Join(dir, "tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`case "$*" in *list-sessions*) printf 'sess-A:\n';; *list-panes*) printf 'pane-A\npane-B\n';; *'-t pane-A'*) exit 1;; esac` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(tmuxPath, []byte(body), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxGraphics(true)
	require.Error(mgr.ApplyTmuxGraphics(t.Context()))

	records := readRecorderArgv(t, record)
	assert.Contains(records, []string{
		"-L", "kenn-forge", "set-option", "-q", "-p", "-u", "-t", "pane-B", "allow-passthrough",
	})
}

func TestManagerApplyTmuxGraphicsDoesNotMutateCustomServer(t *testing.T) {
	require := require.New(t)
	script, record := writeRecorderScript(t)
	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	mgr.SetTmuxGraphics(false)

	require.NoError(mgr.ApplyTmuxGraphics(t.Context()))
	require.NoFileExists(record)
}

func TestManagerApplyTmuxMouseDoesNotMutateCustomServer(t *testing.T) {
	require := require.New(t)
	script, record := writeRecorderScript(t)
	mgr := NewManager(openTestDB(t), t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	mgr.SetTmuxMouse(true)

	require.NoError(mgr.ApplyTmuxMouse(t.Context()))
	require.NoFileExists(record)
}

func TestManagerEnsureTerminalUsesPtyOwnerWhenConfigured(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	script, record := writeRecorderScript(t)
	owner := &fakePtyOwnerClient{}
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetPtyOwnerClient(owner)

	require.NoError(mgr.EnsureTerminal(t.Context(), &db.Workspace{
		TmuxSession:     "sess-owner",
		WorktreePath:    "/tmp/ws",
		TerminalBackend: TerminalBackendPtyOwner,
	}))

	assert.Equal([]fakePtyOwnerCall{{
		Op: "ensure", Session: "sess-owner", Cwd: "/tmp/ws",
	}}, owner.Calls)
	_, err := os.Stat(record)
	assert.True(os.IsNotExist(err))
}

func TestManagerTerminalPaneSnapshotIncludesPtyOwnerTitle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	owner := &fakePtyOwnerClient{
		SnapshotOutput: []byte("recent output"),
		SnapshotTitle:  "⠴ t3code-b5014b03",
	}
	mgr := newTestManager(t, nil, t.TempDir())
	mgr.SetPtyOwnerClient(owner)
	ws := &db.Workspace{
		ID:              "ws-1",
		TmuxSession:     "kenn-forge-ws-1",
		TerminalBackend: TerminalBackendPtyOwner,
	}

	snapshot, err := mgr.TerminalPaneSnapshot(
		context.Background(), ws, ws.TmuxSession,
	)

	require.NoError(err)
	assert.Equal("⠴ t3code-b5014b03", snapshot.Title)
	assert.Equal("recent output", snapshot.Output)
}

func TestManagerCleanupTerminalUsesPtyOwnerForBaseSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	script, record := writeRecorderScript(t)
	owner := &fakePtyOwnerClient{}
	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetPtyOwnerClient(owner)

	ws, err := mgr.Create(t.Context(), "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	_, err = mgr.Delete(t.Context(), ws.ID, true, nil)
	require.NoError(err)

	assert.Equal([]fakePtyOwnerCall{{
		Op: "stop", Session: ws.TmuxSession,
	}}, owner.Calls)
	_, err = os.Stat(record)
	assert.True(os.IsNotExist(err))
}

func TestManagerCleanupPtyOwnerWorkspaceStopsStoredRuntimeTmuxSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	script, record := writeRecorderScript(t)
	owner := &fakePtyOwnerClient{}
	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetPtyOwnerClient(owner)

	ws, err := mgr.Create(t.Context(), "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	recordRuntimeTmuxSessionForTest(
		t, d, ws.ID, "ws-runtime-session", "agent-1",
		"kenn-forge-runtime-session",
		time.Date(2026, 4, 29, 1, 0, 0, 0, time.UTC),
	)

	_, err = mgr.Delete(t.Context(), ws.ID, true, nil)
	require.NoError(err)

	assert.Equal([]fakePtyOwnerCall{{
		Op: "stop", Session: ws.TmuxSession,
	}}, owner.Calls)
	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", "kenn-forge-runtime-session"},
		argvs[0],
	)
	stored, err := d.ListWorkspaceRuntimeTmuxSessions(t.Context(), ws.ID)
	require.NoError(err)
	assert.Empty(stored)
}

func TestManagerDeleteUsesTmuxPrefix(t *testing.T) {
	assert := assert.New(t)

	script, record := writeRecorderScript(t)

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})

	ctx := t.Context()
	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(t, err)

	// force=true skips the dirty-files check. m.clones is nil, so
	// Delete takes the clones==nil short-circuit after killing the
	// tmux session — no git operations are required.
	_, err = mgr.Delete(ctx, ws.ID, true, nil)
	require.NoError(t, err)

	// Delete invokes exactly one tmux command on this path
	// (kill-session). It ignores the exit code because the session
	// may not exist, but our script exits 0 so the invocation is
	// still recorded.
	argvs := readRecorderArgv(t, record)
	require.Len(t, argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", ws.TmuxSession},
		argvs[0],
	)
}

func TestManagerDeleteAllowsMissingTmuxSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "can't find session: missing" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})

	ctx := context.Background()
	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)

	dirty, err := mgr.Delete(ctx, ws.ID, true, nil)
	require.NoError(err)
	assert.Nil(dirty)

	got, err := mgr.Get(ctx, ws.ID)
	require.NoError(err)
	assert.Nil(got)

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", ws.TmuxSession},
		argvs[0],
	)
}

func TestManagerDeleteFailsWhenTmuxKillFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "permission denied" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})

	ctx := context.Background()
	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	require.NoError(d.UpdateWorkspaceStatus(ctx, ws.ID, "ready", nil))

	dirty, err := mgr.Delete(ctx, ws.ID, true, nil)
	assert.Nil(dirty)
	require.Error(err)
	assert.Contains(err.Error(), "kill tmux session")
	assert.Contains(err.Error(), "permission denied")

	got, getErr := mgr.Get(ctx, ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal(ws.ID, got.ID)

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", ws.TmuxSession},
		argvs[0],
	)
}

func TestManagerDeleteStopsBeforeTeardownWhenAdmissionFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	script, record := writeRecorderScript(t)

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})

	ctx := context.Background()
	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)

	admissionErr := errors.New("admission failed")
	dirty, err := mgr.Delete(ctx, ws.ID, true, func(context.Context) error {
		return admissionErr
	})
	assert.Nil(dirty)
	require.ErrorIs(err, admissionErr)

	got, getErr := mgr.Get(ctx, ws.ID)
	require.NoError(getErr)
	require.NotNil(got)
	assert.Equal(ws.ID, got.ID)
	_, statErr := os.Stat(record)
	assert.ErrorIs(statErr, os.ErrNotExist)
}

func TestManagerDeleteTreatsTmuxServerExitDuringKillAsGone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "server exited unexpectedly" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMR(t, d, repoID, 42, "feature/thing")

	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})

	ctx := context.Background()
	ws, err := mgr.Create(ctx, "github", "github.com", "acme", "widget", 42)
	require.NoError(err)
	require.NoError(d.UpdateWorkspaceStatus(ctx, ws.ID, "ready", nil))

	dirty, err := mgr.Delete(ctx, ws.ID, true, nil)
	assert.Nil(dirty)
	require.NoError(err)

	got, getErr := mgr.Get(ctx, ws.ID)
	require.NoError(getErr)
	assert.Nil(got)

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", ws.TmuxSession},
		argvs[0],
	)
}

func TestManagerDeleteAllowsErroredWorkspaceWhenTmuxUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{
		filepath.Join(t.TempDir(), "missing-tmux"),
	})

	ctx := context.Background()
	ws := &Workspace{
		ID:              "ws-tmux-unavailable",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/thing",
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:     "kenn-forge-0000000000000042",
		Status:          "error",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	dirty, err := mgr.Delete(ctx, ws.ID, true, nil)
	require.NoError(err)
	assert.Nil(dirty)

	got, err := mgr.Get(ctx, ws.ID)
	require.NoError(err)
	assert.Nil(got)
}

func TestManagerReapOrphanTmuxSessionsIgnoresUnavailableTmux(t *testing.T) {
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{filepath.Join(t.TempDir(), "missing-tmux")})

	require.NoError(mgr.ReapOrphanTmuxSessions(context.Background()))
}

func TestManagerReapOrphanTmuxSessionsKillsUnknownManagedSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		"TMUX_TEST_OWNER_MARKER=" + shellquote.Join(mgr.tmuxOwnerMarker()) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "list-sessions" ]; then` + "\n" +
		`    printf 'forge-0000000000000001:%s\n' "$TMUX_TEST_OWNER_MARKER"` + "\n" +
		`    printf 'forge-ffffffffffffffff\n'` + "\n" +
		`    printf 'forge-aaaaaaaaaaaaaaaa-0123456789abcdef:%s\n' "$TMUX_TEST_OWNER_MARKER"` + "\n" +
		`    printf 'forge-aaaaaaaaaaaaaaaa-claude:%s\n' "$TMUX_TEST_OWNER_MARKER"` + "\n" +
		`    printf 'forge-notes\nother-session\n'` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))
	mgr.SetTmuxCommand([]string{script, "wrap"})

	live := &Workspace{
		ID:           "ws-live",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
		TmuxSession:  "forge-0000000000000001",
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(context.Background(), live))

	require.NoError(mgr.ReapOrphanTmuxSessions(context.Background()))

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 2)
	assert.Equal(
		[]string{
			"wrap", "list-sessions", "-F",
			"#{session_name}:#{@forge_owner}",
		},
		argvs[0],
	)
	assert.Equal(
		[]string{
			"wrap", "kill-session", "-t",
			"forge-aaaaaaaaaaaaaaaa-0123456789abcdef",
		},
		argvs[1],
	)
	assert.NotContains(argvs, []string{
		"wrap", "show-options", "-qv", "-t",
		"forge-aaaaaaaaaaaaaaaa-claude", "@forge_owner",
	})
	assert.NotContains(argvs, []string{
		"wrap", "kill-session", "-t",
		"forge-aaaaaaaaaaaaaaaa-claude",
	})
}

func TestManagerReapOrphanTmuxSessionsKeepsStoredRuntimeSessions(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	storedHostSession := "forge-host-1111111111111111"
	orphanHostSession := "forge-host-2222222222222222"

	require.NoError(d.InsertWorkspace(context.Background(), &Workspace{
		ID:           "0000000000000001",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
		TmuxSession:  "middleman-0000000000000001",
		Status:       "ready",
	}))
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000001", "0000000000000001_codex",
		"codex", "middleman-0000000000000001-57de4cf40144bdf7",
		time.Time{},
	)
	require.NoError(d.UpsertHostRuntimeTmuxSession(
		context.Background(), &db.HostRuntimeTmuxSession{
			SessionKey:  "host-live",
			SessionName: storedHostSession,
		},
	))
	project, err := d.CreateProject(context.Background(), db.CreateProjectInput{
		DisplayName: "runtime-project",
		LocalPath:   filepath.Join(t.TempDir(), "project"),
	})
	require.NoError(err)
	worktree, err := d.CreateProjectWorktree(
		context.Background(), db.CreateProjectWorktreeInput{
			ProjectID: project.ID,
			Branch:    "feature/runtime",
			Path:      filepath.Join(t.TempDir(), "worktree"),
		},
	)
	require.NoError(err)
	storedProjectSession := "forge-project-worktree-" + worktree.ID +
		"-3333333333333333"
	orphanProjectSession := "forge-project-worktree-unrecorded-4444444444444444"
	require.NoError(d.UpsertProjectWorktreeTmuxSession(
		context.Background(), &db.ProjectWorktreeTmuxSession{
			WorktreeID:  worktree.ID,
			SessionKey:  "project-live",
			SessionName: storedProjectSession,
		},
	))

	owner := mgr.tmuxOwnerMarker()
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "list-sessions" ]; then` + "\n" +
		"    cat <<'SESSIONS'\n" +
		"middleman-0000000000000001:" + owner + "\n" +
		"middleman-0000000000000001-57de4cf40144bdf7:" + owner + "\n" +
		storedHostSession + ":" + owner + "\n" +
		storedProjectSession + ":" + owner + "\n" +
		orphanHostSession + ":" + owner + "\n" +
		orphanProjectSession + ":" + owner + "\n" +
		"forge-aaaaaaaaaaaaaaaa-c857d09db23e6822:" + owner + "\n" +
		"SESSIONS\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))
	mgr.SetTmuxCommand([]string{script, "wrap"})

	require.NoError(mgr.ReapOrphanTmuxSessions(context.Background()))

	argvs := readRecorderArgv(t, record)
	assert.Contains(argvs, []string{
		"wrap", "kill-session", "-t",
		"forge-aaaaaaaaaaaaaaaa-c857d09db23e6822",
	})
	assert.Contains(argvs, []string{
		"wrap", "kill-session", "-t", orphanHostSession,
	})
	assert.Contains(argvs, []string{
		"wrap", "kill-session", "-t", orphanProjectSession,
	})
	assert.NotContains(argvs, []string{
		"wrap", "kill-session", "-t",
		"middleman-0000000000000001-57de4cf40144bdf7",
	})
	assert.NotContains(argvs, []string{
		"wrap", "kill-session", "-t", storedHostSession,
	})
	assert.NotContains(argvs, []string{
		"wrap", "kill-session", "-t", storedProjectSession,
	})
}

func TestManagerPruneMissingTmuxSessionsRemovesStaleRecords(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "list-sessions" ]; then` + "\n" +
		`    printf 'kenn-forge-0000000000000001\nkenn-forge-0000000000000001-57de4cf40144bdf7\n'` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetPtyOwnerFallbackClient(&fakePtyOwnerClient{
		StateSessions: map[string]bool{
			"kenn-forge-0000000000000003": true,
		},
	})
	ctx := context.Background()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "0000000000000001",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
		TmuxSession:  "kenn-forge-0000000000000001",
		Status:       "ready",
	}))
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "0000000000000002",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "gadget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   2,
		GitHeadRef:   "feature/stale",
		WorktreePath: filepath.Join(t.TempDir(), "stale"),
		TmuxSession:  "kenn-forge-0000000000000002",
		Status:       "ready",
	}))
	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "0000000000000003",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "legacy-owner",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   3,
		GitHeadRef:   "feature/owner",
		WorktreePath: filepath.Join(t.TempDir(), "owner"),
		TmuxSession:  "kenn-forge-0000000000000003",
		Status:       "ready",
	}))
	_, err := d.WriteDB().ExecContext(
		ctx,
		`UPDATE forge_workspaces SET terminal_backend = '' WHERE id = ?`,
		"0000000000000003",
	)
	require.NoError(err)
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000001", "0000000000000001_codex",
		"codex", "kenn-forge-0000000000000001-57de4cf40144bdf7",
		time.Time{},
	)
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000001", "0000000000000001_claude",
		"claude", "kenn-forge-0000000000000001-c857d09db23e6822",
		time.Time{},
	)

	pruned, pruneErr := mgr.PruneMissingTmuxSessions(ctx)
	require.NoError(pruneErr)
	assert.True(pruned)

	stored, err := d.ListWorkspaceRuntimeTmuxSessions(ctx, "0000000000000001")
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal(
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
		stored[0].TmuxSession,
	)

	live, err := d.GetWorkspace(ctx, "0000000000000001")
	require.NoError(err)
	require.NotNil(live)
	assert.Equal("ready", live.Status)

	stale, err := d.GetWorkspace(ctx, "0000000000000002")
	require.NoError(err)
	require.NotNil(stale)
	assert.Equal("error", stale.Status)
	require.NotNil(stale.ErrorMessage)
	assert.Contains(*stale.ErrorMessage, "tmux session is no longer running")
	assert.Contains(*stale.ErrorMessage, "kenn-forge-0000000000000002")

	legacyOwner, err := d.GetWorkspace(ctx, "0000000000000003")
	require.NoError(err)
	require.NotNil(legacyOwner)
	assert.Equal("ready", legacyOwner.Status)
}

// TestManagerTmuxSessionListSurvivesTmux36Sanitization guards against
// tmux 3.6+'s format sanitization: control characters in -F output print
// as "_", so a literal tab separator corrupts every line into
// "<name>_<owner>", making prune mark live workspaces as errored and reap
// skip owned orphans. The fake tmux expands the requested format the way
// tmux 3.6 does, including the tab-to-underscore substitution, so this
// fails if the list format ever reverts to a control-character separator.
func TestManagerTmuxSessionListSurvivesTmux36Sanitization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		"TMUX_TEST_OWNER_MARKER=" + shellquote.Join(mgr.tmuxOwnerMarker()) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`fmt=''` + "\n" +
		`prev=''` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$prev" = "-F" ]; then fmt="$a"; fi` + "\n" +
		`  prev="$a"` + "\n" +
		`done` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "list-sessions" ]; then` + "\n" +
		`    for name in middleman-0000000000000001 forge-aaaaaaaaaaaaaaaa; do` + "\n" +
		`      printf '%s\n' "$fmt" \` + "\n" +
		`        | sed -e "s|#{session_name}|$name|" \` + "\n" +
		`              -e "s|#{@forge_owner}|$TMUX_TEST_OWNER_MARKER|" \` + "\n" +
		`        | tr '\t' '_'` + "\n" +
		`    done` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		`done` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))
	mgr.SetTmuxCommand([]string{script, "wrap"})
	ctx := context.Background()

	require.NoError(d.InsertWorkspace(ctx, &Workspace{
		ID:           "0000000000000001",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
		TmuxSession:  "middleman-0000000000000001",
		Status:       "ready",
	}))

	pruned, pruneErr := mgr.PruneMissingTmuxSessions(ctx)
	require.NoError(pruneErr)
	assert.False(pruned, "no-op prune must report no change so callers stay silent")
	live, err := d.GetWorkspace(ctx, "0000000000000001")
	require.NoError(err)
	require.NotNil(live)
	assert.Equal("ready", live.Status)

	require.NoError(mgr.ReapOrphanTmuxSessions(ctx))
	argvs := readRecorderArgv(t, record)
	assert.Contains(argvs, []string{
		"wrap", "kill-session", "-t", "forge-aaaaaaaaaaaaaaaa",
	})
	assert.NotContains(argvs, []string{
		"wrap", "kill-session", "-t", "middleman-0000000000000001",
	})
}

// TestManagerListTmuxSessionInfosRealTmux lists sessions through an
// actual tmux server on an isolated socket. tmux 3.6+ sanitizes control
// characters in -F output, which silently broke a tab-separated list
// format on real servers while fake-script tests kept passing; running
// against the installed binary catches any future sanitization drift.
func TestManagerListTmuxSessionInfosRealTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("real tmux listing test uses Unix tmux")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux unavailable in this test environment: %v", err)
	}

	assert := assert.New(t)
	require := require.New(t)

	tmuxCommand := privateTmuxOwner.Command(t, tmuxPath)
	run := func(args ...string) {
		t.Helper()
		cmd := procutil.Command(
			tmuxCommand[0],
			append(append([]string(nil), tmuxCommand[1:]...), args...)...,
		)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		out, err := cmd.CombinedOutput()
		require.NoError(err, string(out))
	}

	const owned = "kenn-forge-0123456789abcdef"
	const unowned = "kenn-forge-fedcba9876543210"
	run("new-session", "-d", "-s", owned, "sleep 30")
	run("new-session", "-d", "-s", unowned, "sleep 30")

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand(tmuxCommand)
	run("set-option", "-t", owned, "@forge_owner", mgr.tmuxOwnerMarker())

	infos, err := mgr.listTmuxSessionInfos(context.Background())
	require.NoError(err)
	owners := make(map[string]string, len(infos))
	for _, info := range infos {
		owners[info.name] = info.owner
	}
	require.Len(owners, 2)
	assert.Equal(mgr.tmuxOwnerMarker(), owners[owned])
	assert.Contains(owners, unowned)
	assert.Empty(owners[unowned])
}

func TestManagerTmuxSessionsForWorkspaceReadsStoredRuntimeSessions(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	require.NoError(d.InsertWorkspace(context.Background(), &Workspace{
		ID:           "0000000000000001",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
		TmuxSession:  "kenn-forge-0000000000000001",
		Status:       "ready",
	}))
	createdAt := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000001", "0000000000000001_codex",
		"codex", "kenn-forge-0000000000000001-57de4cf40144bdf7",
		createdAt.Add(time.Minute),
	)
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000001", "0000000000000001_claude",
		"claude", "kenn-forge-0000000000000001-c857d09db23e6822",
		createdAt,
	)
	require.NoError(d.InsertWorkspace(context.Background(), &Workspace{
		ID:           "0000000000000002",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "gadget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   2,
		GitHeadRef:   "feature/other",
		WorktreePath: filepath.Join(t.TempDir(), "other"),
		TmuxSession:  "kenn-forge-0000000000000002",
		Status:       "ready",
	}))
	recordRuntimeTmuxSessionForTest(
		t, d, "0000000000000002", "0000000000000002_codex",
		"codex", "kenn-forge-0000000000000002-57de4cf40144bdf7",
		createdAt,
	)

	sessions, err := mgr.TmuxSessionsForWorkspace(
		context.Background(),
		"0000000000000001",
		"kenn-forge-0000000000000001",
	)
	require.NoError(err)

	assert.Equal([]string{
		"kenn-forge-0000000000000001",
		"kenn-forge-0000000000000001-c857d09db23e6822",
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
	}, sessions)

	sessions, err = mgr.TmuxSessionsForWorkspace(
		context.Background(),
		"0000000000000001",
		"",
	)
	require.NoError(err)
	assert.Equal([]string{
		"kenn-forge-0000000000000001-c857d09db23e6822",
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
	}, sessions)
}

func TestManagerCleanupTmuxSessionKillsRuntimeSessionsForWorkspace(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	ws := &Workspace{
		ID:           "0000000000000001",
		TmuxSession:  "kenn-forge-0000000000000001",
		Status:       "ready",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
	}
	require.NoError(d.InsertWorkspace(context.Background(), ws))
	recordRuntimeTmuxSessionForTest(
		t, d, ws.ID, "0000000000000001_codex", "codex",
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
		time.Time{},
	)
	recordRuntimeTmuxSessionForTest(
		t, d, ws.ID, "0000000000000001_claude", "claude",
		"kenn-forge-0000000000000001-c857d09db23e6822",
		time.Time{},
	)

	require.NoError(mgr.cleanupTmuxSession(context.Background(), ws))

	argvs := readRecorderArgv(t, record)
	assert.Contains(argvs, []string{
		"kill-session", "-t", "kenn-forge-0000000000000001",
	})
	assert.Contains(argvs, []string{
		"kill-session", "-t",
		"kenn-forge-0000000000000001-c857d09db23e6822",
	})
	assert.Contains(argvs, []string{
		"kill-session", "-t",
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
	})
	assert.NotContains(argvs, []string{
		"kill-session", "-t",
		"kenn-forge-0000000000000002-57de4cf40144bdf7",
	})
	stored, err := d.ListWorkspaceRuntimeTmuxSessions(context.Background(), ws.ID)
	require.NoError(err)
	assert.Empty(stored)
}

func TestManagerCleanupTmuxSessionPreservesStoredRowsAfterRuntimeKillFailure(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`target=""` + "\n" +
		`prev=""` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$prev" = "-t" ]; then target="$a"; fi` + "\n" +
		`  prev="$a"` + "\n" +
		`done` + "\n" +
		`if [ "$1" = "kill-session" ]; then` + "\n" +
		`  case "$target" in` + "\n" +
		`    kenn-forge-0000000000000001)` + "\n" +
		`      echo "can't find session: $target" >&2` + "\n" +
		`      exit 1` + "\n" +
		`      ;;` + "\n" +
		`    kenn-forge-0000000000000001-57de4cf40144bdf7)` + "\n" +
		`      echo "permission denied" >&2` + "\n" +
		`      exit 42` + "\n" +
		`      ;;` + "\n" +
		`  esac` + "\n" +
		`fi` + "\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	ws := &Workspace{
		ID:           "0000000000000001",
		TmuxSession:  "kenn-forge-0000000000000001",
		Status:       "error",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
	}
	require.NoError(d.InsertWorkspace(context.Background(), ws))
	for _, targetKey := range []string{"codex", "claude"} {
		recordRuntimeTmuxSessionForTest(
			t,
			d,
			ws.ID,
			ws.ID+"_"+targetKey,
			targetKey,
			map[string]string{
				"codex":  "kenn-forge-0000000000000001-57de4cf40144bdf7",
				"claude": "kenn-forge-0000000000000001-c857d09db23e6822",
			}[targetKey],
			time.Time{},
		)
	}

	err := mgr.cleanupTmuxSession(context.Background(), ws)
	require.Error(err)
	assert.Contains(err.Error(), "kenn-forge-0000000000000001-57de4cf40144bdf7")

	argvs := readRecorderArgv(t, record)
	assert.Contains(argvs, []string{
		"kill-session", "-t", "kenn-forge-0000000000000001",
	})
	assert.Contains(argvs, []string{
		"kill-session", "-t",
		"kenn-forge-0000000000000001-57de4cf40144bdf7",
	})
	assert.Contains(argvs, []string{
		"kill-session", "-t",
		"kenn-forge-0000000000000001-c857d09db23e6822",
	})

	stored, err := d.ListWorkspaceRuntimeTmuxSessions(context.Background(), ws.ID)
	require.NoError(err)
	require.Len(stored, 2)
}

func TestManagerForgetRuntimeSessionCreatedAtPreservesRecreatedRow(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	require.NoError(d.InsertWorkspace(context.Background(), &Workspace{
		ID:           "ws-1",
		TmuxSession:  "kenn-forge-ws-1",
		Status:       "ready",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
	}))
	oldCreatedAt := time.Date(2026, 4, 29, 1, 0, 0, 0, time.UTC)
	newCreatedAt := time.Date(2026, 4, 29, 1, 1, 0, 0, time.UTC)
	sessionKey := "ws-1_helper"
	recordRuntimeTmuxSessionForTest(
		t, d, "ws-1", sessionKey, "helper", "kenn-forge-ws-1-helper",
		oldCreatedAt,
	)
	recordRuntimeTmuxSessionForTest(
		t, d, "ws-1", sessionKey, "helper", "kenn-forge-ws-1-helper",
		newCreatedAt,
	)

	deleted, err := mgr.ForgetRuntimeSessionCreatedAt(
		context.Background(), "ws-1", sessionKey, oldCreatedAt,
	)
	require.NoError(err)
	assert.False(deleted)

	stored, err := d.ListWorkspaceRuntimeTmuxSessions(context.Background(), "ws-1")
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal(newCreatedAt, stored[0].CreatedAt)
}

func TestManagerForgetRuntimeSessionAfterExitKeepsLiveTmuxSession(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	require.NoError(d.InsertWorkspace(context.Background(), &Workspace{
		ID:           "ws-1",
		TmuxSession:  "kenn-forge-ws-1",
		Status:       "ready",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   1,
		GitHeadRef:   "feature/live",
		WorktreePath: filepath.Join(t.TempDir(), "live"),
	}))
	createdAt := time.Date(2026, 4, 29, 1, 0, 0, 0, time.UTC)
	sessionKey := "ws-1_helper"
	tmuxSession := "kenn-forge-ws-1-helper"
	recordRuntimeTmuxSessionForTest(
		t, d, "ws-1", sessionKey, "helper", tmuxSession, createdAt,
	)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	existsFile := filepath.Join(dir, "exists")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		"TMUX_EXISTS_FILE=" + shellquote.Join(existsFile) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`if [ "$1" = "has-session" ]; then` + "\n" +
		`  if [ -f "$TMUX_EXISTS_FILE" ]; then exit 0; fi` + "\n" +
		`  echo "can't find session: $3" >&2` + "\n" +
		`  exit 1` + "\n" +
		`fi` + "\n" +
		`exit 0` + "\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))
	mgr.SetTmuxCommand([]string{script})

	require.NoError(os.WriteFile(existsFile, []byte("1"), 0o644))
	deleted, err := mgr.ForgetRuntimeSessionAfterExit(
		context.Background(), "ws-1", sessionKey, createdAt, tmuxSession,
	)
	require.NoError(err)
	assert.False(deleted)
	stored, err := d.ListWorkspaceRuntimeTmuxSessions(context.Background(), "ws-1")
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal(sessionKey, stored[0].SessionKey)

	require.NoError(os.Remove(existsFile))
	deleted, err = mgr.ForgetRuntimeSessionAfterExit(
		context.Background(), "ws-1", sessionKey, createdAt, tmuxSession,
	)
	require.NoError(err)
	assert.True(deleted)
	stored, err = d.ListWorkspaceRuntimeTmuxSessions(context.Background(), "ws-1")
	require.NoError(err)
	assert.Empty(stored)

	argvs := readRecorderArgv(t, record)
	assert.Contains(argvs, []string{"has-session", "-t", tmuxSession})
}

func TestManagerRequestRetryFailsWhenTmuxCleanupFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "permission denied" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	ctx := context.Background()
	errMsg := "tmux new-session failed"
	ws := &Workspace{
		ID:              "ws-retry-cleanup-fails",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-retry-cleanup-fails",
		TmuxSession:     "kenn-forge-retry-cleanup-fails",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))
	require.NoError(d.InsertWorkspaceSetupEvent(ctx, &db.WorkspaceSetupEvent{
		WorkspaceID: ws.ID,
		Stage:       workspaceSetupStageTmuxSession,
		Outcome:     "success",
		Message:     "tmux session started",
	}))

	next, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	assert.Nil(next)
	assert.False(startNow)
	require.Error(err)
	assert.Contains(err.Error(), "cleanup workspace artifacts before retry")
	assert.Contains(err.Error(), "kill tmux session")
	assert.Contains(err.Error(), "permission denied")

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	require.NotNil(got.ErrorMessage)
	assert.Contains(*got.ErrorMessage, "permission denied")
	assert.Equal("kenn-forge/pr-42", got.WorkspaceBranch)

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 1)
	assert.Equal(
		[]string{"wrap", "kill-session", "-t", ws.TmuxSession},
		argvs[0],
	)
}

func TestManagerRequestRetryConsumesQueuedRetryWhenCleanupFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	count := filepath.Join(dir, "count")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_STARTED=" + shellquote.Join(started) + "\n" +
		"TMUX_RELEASE=" + shellquote.Join(release) + "\n" +
		"TMUX_COUNT=" + shellquote.Join(count) + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    n=0` + "\n" +
		`    if [ -f "$TMUX_COUNT" ]; then n=$(cat "$TMUX_COUNT"); fi` + "\n" +
		`    n=$((n + 1))` + "\n" +
		`    printf '%s' "$n" > "$TMUX_COUNT"` + "\n" +
		`    if [ "$n" -eq 1 ]; then` + "\n" +
		`      : > "$TMUX_STARTED"` + "\n" +
		`      while [ ! -f "$TMUX_RELEASE" ]; do sleep 0.01; done` + "\n" +
		`      echo "permission denied" >&2` + "\n" +
		`      exit 1` + "\n" +
		`    fi` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	ctx := context.Background()
	errMsg := "tmux new-session failed"
	ws := &Workspace{
		ID:              "ws-retry-cleanup-queued",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-retry-cleanup-queued",
		TmuxSession:     "kenn-forge-retry-cleanup-queued",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))
	require.NoError(d.InsertWorkspaceSetupEvent(ctx, &db.WorkspaceSetupEvent{
		WorkspaceID: ws.ID,
		Stage:       workspaceSetupStageTmuxSession,
		Outcome:     "success",
		Message:     "tmux session started",
	}))

	type retryResult struct {
		ws       *Workspace
		startNow bool
		err      error
	}
	firstResult := make(chan retryResult, 1)
	go func() {
		next, startNow, err := mgr.RequestRetry(ctx, ws.ID)
		firstResult <- retryResult{ws: next, startNow: startNow, err: err}
	}()

	const retryWait = 5 * time.Second

	require.Eventually(func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, retryWait, 10*time.Millisecond)
	require.Eventually(func() bool {
		got, err := d.GetWorkspace(ctx, ws.ID)
		return err == nil && got != nil && got.Status == "creating"
	}, retryWait, 10*time.Millisecond)

	queuedWS, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(queuedWS)
	assert.False(startNow)
	assert.Equal("creating", queuedWS.Status)

	require.NoError(os.WriteFile(release, []byte("1"), 0o644))
	var first retryResult
	require.Eventually(func() bool {
		select {
		case first = <-firstResult:
			return true
		default:
			return false
		}
	}, retryWait, 10*time.Millisecond)
	assert.Nil(first.ws)
	assert.False(first.startNow)
	require.Error(first.err)
	assert.Contains(first.err.Error(), "permission denied")

	next, queued, err := mgr.StartQueuedRetryIfErrored(ctx, ws.ID)
	require.NoError(err)
	assert.Nil(next)
	assert.False(queued)

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("error", got.Status)
}

func TestManagerRequestRetrySkipsGitCleanupWhenCloneMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "can't find session: missing" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script, "wrap"})
	mgr.SetClones(gitclone.New(filepath.Join(dir, "clones"), nil))
	ctx := context.Background()
	errMsg := "ensure clone failed"
	ws := &Workspace{
		ID:              "ws-retry-missing-clone",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    filepath.Join(dir, "missing-worktree"),
		TmuxSession:     "kenn-forge-retry-missing-clone",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	next, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.True(startNow)
	assert.Equal("creating", next.Status)
	assert.Equal(workspaceBranchUnknown, next.WorkspaceBranch)

	got, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("creating", got.Status)
	assert.Equal(workspaceBranchUnknown, got.WorkspaceBranch)
	assert.Nil(got.ErrorMessage)
}

func TestIssueRetryCleansLeakedUnknownBranchAndUsesIssueBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	host, owner, name := "github.com", "acme", "widget"
	baseDir := t.TempDir()
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetClones(gitclone.New(baseDir, nil))

	cloneDir, err := mgr.clones.ClonePath("github", host, owner, name)
	require.NoError(err)
	seedWorkspaceBareCloneAt(t, cloneDir)
	configureOriginHeadForIssueWorkspace(t, cloneDir)

	staleWorktree := filepath.Join(t.TempDir(), "stale-unknown-worktree")
	runWorkspaceTestGit(
		t, cloneDir,
		"worktree", "add", staleWorktree,
		"-b", workspaceBranchUnknown, "origin/HEAD",
	)
	exists, err := localBranchExists(
		t.Context(), cloneDir, workspaceBranchUnknown,
	)
	require.NoError(err)
	require.True(exists)

	ws := &Workspace{
		ID:              "ws-issue-retry-unknown",
		PlatformHost:    host,
		RepoOwner:       owner,
		RepoName:        name,
		ItemType:        db.WorkspaceItemTypeIssue,
		ItemNumber:      23,
		GitHeadRef:      "kenn-forge/issue-23-federation-test",
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    staleWorktree,
		Status:          "error",
	}
	require.NoError(writeWorkspaceOwnershipMarker(t.Context(), cloneDir, ws))
	require.NoError(mgr.cleanupWorkspaceArtifactsForRetry(t.Context(), ws))

	exists, err = localBranchExists(
		t.Context(), cloneDir, workspaceBranchUnknown,
	)
	require.NoError(err)
	assert.False(exists)

	branch, err := mgr.addIssueWorktree(t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws)
	require.NoError(err)
	assert.Equal(ws.GitHeadRef, branch)

	exists, err = localBranchExists(t.Context(), cloneDir, ws.GitHeadRef)
	require.NoError(err)
	assert.True(exists)
	exists, err = localBranchExists(
		t.Context(), cloneDir, workspaceBranchUnknown,
	)
	require.NoError(err)
	assert.False(exists)
}

func TestManagerRequestRetryQueuesWhileCreatingAndStartsIfErrored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	ctx := context.Background()
	ws := &Workspace{
		ID:              "ws-queued-retry",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    "/tmp/ws-queued-retry",
		TmuxSession:     "kenn-forge-ws-queued-retry",
		Status:          "creating",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	current, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(current)
	assert.False(startNow)
	assert.Equal("creating", current.Status)

	errMsg := "ensure clone failed"
	require.NoError(d.UpdateWorkspaceStatus(ctx, ws.ID, "error", &errMsg))

	next, queued, err := mgr.StartQueuedRetryIfErrored(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.True(queued)
	assert.Equal("creating", next.Status)
	assert.Nil(next.ErrorMessage)

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("creating", stored.Status)
	assert.Nil(stored.ErrorMessage)
	assert.Equal(workspaceBranchUnknown, stored.WorkspaceBranch)

	next, queued, err = mgr.StartQueuedRetryIfErrored(ctx, ws.ID)
	require.NoError(err)
	assert.Nil(next)
	assert.False(queued)
}

func TestManagerRequestRetryPreservesReusedIssueBranchSentinel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	ctx := context.Background()
	errMsg := "setup failed"
	ws := &Workspace{
		ID:              "ws-reused-issue-retry",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeIssue,
		ItemNumber:      7,
		GitHeadRef:      "feature/reused",
		WorkspaceBranch: "",
		WorktreePath:    "/tmp/ws-reused-issue-retry",
		TmuxSession:     "kenn-forge-ws-reused-issue-retry",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	next, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.True(startNow)
	assert.Equal("creating", next.Status)
	assert.Empty(next.WorkspaceBranch)
	assert.Nil(next.ErrorMessage)

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("creating", stored.Status)
	assert.Empty(stored.WorkspaceBranch)
	assert.Nil(stored.ErrorMessage)
}

func TestManagerRequestRetryStartsWhenSetupFailedBeforeQueue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	ctx := context.Background()
	errMsg := "ensure clone failed"
	ws := &Workspace{
		ID:              "ws-raced-retry",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath:    "/tmp/ws-raced-retry",
		TmuxSession:     "kenn-forge-ws-raced-retry",
		Status:          "error",
		ErrorMessage:    &errMsg,
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	next, startNow, err := mgr.queueRetryOrStartErrored(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.True(startNow)
	assert.Equal("creating", next.Status)
	assert.Nil(next.ErrorMessage)
	assert.Equal(workspaceBranchUnknown, next.WorkspaceBranch)

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("creating", stored.Status)
	assert.Nil(stored.ErrorMessage)
	assert.Equal(workspaceBranchUnknown, stored.WorkspaceBranch)

	next, queued, err := mgr.StartQueuedRetryIfErrored(ctx, ws.ID)
	require.NoError(err)
	assert.Nil(next)
	assert.False(queued)
}

func TestManagerRequestRetryDiscardsQueuedRetryWhenSetupSucceeds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	ctx := context.Background()
	ws := &Workspace{
		ID:              "ws-discard-retry",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypePullRequest,
		ItemNumber:      42,
		GitHeadRef:      "feature/retry",
		WorkspaceBranch: workspaceBranchUnknown,
		WorktreePath:    "/tmp/ws-discard-retry",
		TmuxSession:     "kenn-forge-ws-discard-retry",
		Status:          "creating",
	}
	require.NoError(d.InsertWorkspace(ctx, ws))

	current, startNow, err := mgr.RequestRetry(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(current)
	assert.False(startNow)

	require.NoError(d.UpdateWorkspaceStatus(ctx, ws.ID, "ready", nil))

	next, queued, err := mgr.StartQueuedRetryIfErrored(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(next)
	assert.False(queued)
	assert.Equal("ready", next.Status)

	stored, err := d.GetWorkspace(ctx, ws.ID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("ready", stored.Status)
}

func TestManagerEnsureTmuxCreatesSessionOnMiss(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Script: "has-session" emits tmux's canonical "can't find
	// session" stderr and exits 1 (so isTmuxSessionAbsent classifies
	// it as session-missing rather than wrapper failure); everything
	// else succeeds, so EnsureTmux calls newTmuxSession.
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	mgr.SetHideTmuxStatus(true)
	mgr.SetTmuxGraphics(true)
	mgr.SetTmuxMouse(true)

	require.NoError(mgr.EnsureTmux(t.Context(), "sess-B", "/tmp/cwd"))

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 5)
	assert.Equal(
		[]string{"has-session", "-t", "sess-B"},
		argvs[0],
	)
	// new-session argv: "new-session -d -s sess-B -c /tmp/cwd <handoff>"
	// where <handoff> is the /bin/sh env-file bootstrap that delivers
	// the credential-sanitized environment and login shell to the pane.
	require.Len(argvs[1], 7)
	assert.Equal("new-session", argvs[1][0])
	assert.Equal("-d", argvs[1][1])
	assert.Equal("-s", argvs[1][2])
	assert.Equal("sess-B", argvs[1][3])
	assert.Equal("-c", argvs[1][4])
	assert.Equal("/tmp/cwd", argvs[1][5])
	assert.Contains(argvs[1][6], "/bin/sh ")
	assert.Equal(
		[]string{
			"set-option", "-t", "sess-B",
			"@forge_owner", mgr.tmuxOwnerMarker(),
		},
		argvs[2],
	)
	assert.Equal(
		[]string{
			"set-option", "-q", "-p", "-t", "sess-B",
			"allow-passthrough", "on",
		},
		argvs[3],
	)
	assert.Equal(
		[]string{"set-option", "-q", "-t", "sess-B", "status", "off"},
		argvs[4],
	)
}

func TestManagerEnsureTmuxCreatesSessionOnMacOSMissingServer(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "error connecting to /private/tmp/tmux-501/default (No such file or directory)" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})
	mgr.SetTmuxGraphics(true)
	mgr.SetTmuxMouse(true)

	require.NoError(mgr.EnsureTmux(context.Background(), "sess-macos", "/tmp/cwd"))

	argvs := readRecorderArgv(t, record)
	require.Len(argvs, 4)
	assert.Equal(
		[]string{"has-session", "-t", "sess-macos"},
		argvs[0],
	)
	assert.Equal("new-session", argvs[1][0])
	assert.Equal("sess-macos", argvs[1][3])
	assert.Equal(
		[]string{
			"set-option", "-t", "sess-macos",
			"@forge_owner", mgr.tmuxOwnerMarker(),
		},
		argvs[2],
	)
	assert.Equal(
		[]string{
			"set-option", "-q", "-p", "-t", "sess-macos",
			"allow-passthrough", "on",
		},
		argvs[3],
	)
}

// TestReadRecorderArgvPreservesEmptyArgs pins down the parser's
// empty-arg handling. The NUL-delimited record format was chosen to
// round-trip argv with empty-string elements unambiguously; the
// parser must keep interior and trailing empties rather than
// collapsing them.
func TestReadRecorderArgvPreservesEmptyArgs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record")

	// First record: 3 args with an interior empty ("a", "", "b").
	// Second record: 2 args with a trailing empty ("x", "").
	body := "3\x00a\x00\x00b\x00" + "2\x00x\x00\x00"
	require.NoError(os.WriteFile(path, []byte(body), 0o644))

	argvs := readRecorderArgv(t, path)
	require.Len(argvs, 2)
	assert.Equal([]string{"a", "", "b"}, argvs[0])
	assert.Equal([]string{"x", ""}, argvs[1])
}

// TestManagerEnsureTmuxPropagatesBinaryError verifies that a wrapper
// misconfiguration (binary not on disk) surfaces as an error rather
// than being silently conflated with "session does not exist, please
// create one." The previous boolean-only tmuxSessionExists swallowed
// this case — EnsureTmux would proceed to run new-session with the
// same broken wrapper and the error would only surface on the second
// exec, masking the real cause.
func TestManagerEnsureTmuxPropagatesBinaryError(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	// Path that cannot possibly exist — exec returns a non-exit
	// error (ENOENT), not an *exec.ExitError.
	mgr.SetTmuxCommand(
		[]string{filepath.Join(t.TempDir(), "does-not-exist")},
	)

	err := mgr.EnsureTmux(t.Context(), "sess-X", "/tmp")
	require.Error(err)
	require.Contains(err.Error(), "tmux has-session")
}

// TestManagerEnsureTmuxPropagatesNon1ExitCode pins down the
// exit-code-1 carve-out in tmuxSessionExists. tmux's has-session
// exits 1 specifically when the session is not found; wrappers that
// fail for their own reasons typically exit with other codes (127
// "command not found", 203 "exec failed", etc.). A wrapper exiting
// with a non-1 code used to be silently treated as "session absent"
// because the old check matched any *exec.ExitError. Now it must
// propagate to the caller so misconfiguration surfaces cleanly.
func TestManagerEnsureTmuxPropagatesNon1ExitCode(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	// exit 127 mimics "command not found" — a common wrapper failure
	// signal that is NOT tmux's own "session missing" response.
	body := "#!/bin/sh\nexit 127\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})

	err := mgr.EnsureTmux(t.Context(), "sess-Y", "/tmp")
	require.Error(err)
	require.Contains(err.Error(), "tmux has-session")
}

// TestManagerEnsureTmuxPropagatesExit1NonTmuxError covers the
// second half of the session-absent heuristic: exit code 1 alone is
// not enough, the output must match tmux's canonical "session
// missing" phrases too. Many real wrappers and shell scripts use
// exit 1 as a generic failure signal — treating that as "session
// absent" would mask the wrapper bug by immediately trying
// new-session through the same broken wrapper.
func TestManagerEnsureTmuxPropagatesExit1NonTmuxError(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\necho 'wrapper blew up' >&2\nexit 1\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})

	err := mgr.EnsureTmux(t.Context(), "sess-Q", "/tmp")
	require.Error(err)
	require.Contains(err.Error(), "tmux has-session")
	require.Contains(err.Error(), "wrapper blew up")
}

// TestManagerEnsureTmuxIgnoresAbsencePhraseOnStdout pins down the
// stdout vs. stderr distinction. A wrapper that exits 1 with the
// tmux phrase on stdout (e.g. one that mirrors stderr to stdout for
// logging, or a script that coincidentally prints the phrase for
// unrelated reasons) must NOT be treated as session-absent — only
// stderr carries the authoritative tmux signal.
func TestManagerEnsureTmuxIgnoresAbsencePhraseOnStdout(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`echo "can't find session: sim"` + "\n" + // stdout only
		`echo "real failure" >&2` + "\n" +
		"exit 1\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	d := openTestDB(t)
	mgr := newTestManager(t, d, t.TempDir())
	mgr.SetTmuxCommand([]string{script})

	err := mgr.EnsureTmux(t.Context(), "sess-R", "/tmp")
	require.Error(err)
	require.Contains(err.Error(), "tmux has-session")
	require.Contains(err.Error(), "real failure")
}

type fakePtyOwnerCall struct {
	Op      string
	Session string
	Cwd     string
}

type fakePtyOwnerClient struct {
	Calls          []fakePtyOwnerCall
	StateExists    bool
	StateSessions  map[string]bool
	SnapshotOutput []byte
	SnapshotTitle  string
}

func (f *fakePtyOwnerClient) HasState(session string) bool {
	return f.StateExists || f.StateSessions[session]
}

func (f *fakePtyOwnerClient) Ensure(
	_ context.Context,
	session string,
	cwd string,
) error {
	f.Calls = append(f.Calls, fakePtyOwnerCall{
		Op: "ensure", Session: session, Cwd: cwd,
	})
	return nil
}

func (f *fakePtyOwnerClient) Attach(
	context.Context,
	string,
	ptysize.Geometry,
) (*ptyowner.Attachment, error) {
	return nil, nil
}

func (f *fakePtyOwnerClient) Stop(
	_ context.Context,
	session string,
) error {
	f.Calls = append(f.Calls, fakePtyOwnerCall{
		Op: "stop", Session: session,
	})
	return nil
}

func (f *fakePtyOwnerClient) Snapshot(
	context.Context,
	string,
) (ptyowner.Status, error) {
	return ptyowner.Status{
		Output: f.SnapshotOutput,
		Title:  f.SnapshotTitle,
	}, nil
}

func TestWorkspaceBranchCandidatesDoesNotIncludeBareForSluggedWorkspace(t *testing.T) {
	// Slug-style issue workspace whose bare-form branch name might
	// be a user-owned local branch unrelated to kenn-forge. Cleanup
	// must return only the persisted GitHeadRef so the unrelated
	// branch is not deleted.
	assert := assert.New(t)
	ws := &Workspace{
		ItemType:   db.WorkspaceItemTypeIssue,
		ItemNumber: 10,
		GitHeadRef: "kenn-forge/issue-10-widget-rendering-broken",
	}
	got := workspaceBranchCandidates(ws, workspaceBranchUnknown)
	assert.Equal([]string{"kenn-forge/issue-10-widget-rendering-broken"}, got)
}

func TestWorkspaceBranchCandidatesUsesBareFallbackOnlyForLegacyWorkspace(t *testing.T) {
	// Pre-feature issue workspaces have no recorded GitHeadRef.
	// Cleanup must still find the bare kenn-forge/issue-<n> branch
	// those workspaces actually use.
	assert := assert.New(t)
	ws := &Workspace{
		ItemType:   db.WorkspaceItemTypeIssue,
		ItemNumber: 10,
		GitHeadRef: "",
	}
	got := workspaceBranchCandidates(ws, workspaceBranchUnknown)
	assert.Equal([]string{"kenn-forge/issue-10"}, got)
}

func TestIsGitWorktreeAbsentClassifiesCorruptGitfile(t *testing.T) {
	// A "git worktree add" interrupted mid-write (e.g. the daemon
	// canceling background setup at shutdown) leaves a worktree whose
	// .git gitfile is empty or partial. Cleanup must treat such a
	// dead worktree as absent rather than failing, so the workspace
	// stays deletable. These are the verbatim phrases git emits,
	// wrapped the way runGit wraps subprocess failures.
	assert := assert.New(t)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"rev-parse on corrupt gitfile",
			fmt.Errorf(
				"%w: %s", errors.New("exit status 128"),
				"fatal: invalid gitfile format: /tmp/wt/.git",
			),
			true,
		},
		{
			"worktree remove on corrupt gitfile",
			fmt.Errorf(
				"%w: %s", errors.New("exit status 128"),
				"fatal: validation failed, cannot remove working "+
					"tree: '/tmp/wt/.git' is not a .git file, error code 5",
			),
			true,
		},
		{
			"missing worktree directory",
			errors.New("fatal: '/tmp/wt' is not a working tree"),
			true,
		},
		{
			"missing linked-worktree metadata",
			fmt.Errorf(
				"%w: %s", errors.New("exit status 128"),
				"fatal: failed to read worktrees/pr-1/commondir: Success",
			),
			true,
		},
		{"nil error", nil, false},
		{
			"unrelated git failure",
			fmt.Errorf(
				"%w: %s", errors.New("exit status 128"),
				"fatal: could not read Username for 'https://github.com'",
			),
			false,
		},
	}
	for _, tc := range cases {
		assert.Equalf(
			tc.want, isGitWorktreeAbsent(tc.err), "case %s", tc.name,
		)
	}
}

func TestFileLockManagerAcquireRelease(t *testing.T) {
	require := require.New(t)
	mgr := NewFileLockManager()
	ctx := t.Context()
	repo := t.TempDir()

	first, err := mgr.Acquire(ctx, repo)
	require.NoError(err)
	require.NoError(first.Unlock())

	second, err := mgr.Acquire(ctx, repo)
	require.NoError(err)
	require.NoError(second.Unlock())
}

func TestFileLockManagerSerializesGoroutines(t *testing.T) {
	require := require.New(t)
	mgr := NewFileLockManager()
	ctx := t.Context()
	repo := t.TempDir()

	const goroutines = 6
	var inCritical atomic.Int32
	var maxObserved atomic.Int32
	var overlap atomic.Int32

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			lock, err := mgr.Acquire(ctx, repo)
			if err != nil {
				return
			}
			defer func() { _ = lock.Unlock() }()
			current := inCritical.Add(1)
			defer inCritical.Add(-1)
			if current > 1 {
				overlap.Add(1)
			}
			for {
				prev := maxObserved.Load()
				if current <= prev || maxObserved.CompareAndSwap(prev, current) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
		})
	}
	wg.Wait()

	require.Equal(int32(1), maxObserved.Load(),
		"only one goroutine should hold the lock at a time")
	require.Equal(int32(0), overlap.Load(),
		"no goroutine should observe another holder in its critical section")
	require.Equal(int32(0), inCritical.Load())
}

func TestFileLockManagerCtxCancelWhileWaiting(t *testing.T) {
	require := require.New(t)
	mgr := NewFileLockManager()
	repo := t.TempDir()

	held, err := mgr.Acquire(t.Context(), repo)
	require.NoError(err)
	defer func() { _ = held.Unlock() }()

	ctx, cancel := context.WithCancel(t.Context())
	gotErr := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := mgr.Acquire(ctx, repo)
		gotErr <- err
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-gotErr:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		require.FailNow("Acquire did not return after ctx cancel")
	}
}

func TestFileLockManagerDoubleUnlock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mgr := NewFileLockManager()
	lock, err := mgr.Acquire(t.Context(), t.TempDir())
	require.NoError(err)
	require.NoError(lock.Unlock())
	assert.Error(lock.Unlock())
}

func TestManagerWithRepoLockReleaseOnSuccess(t *testing.T) {
	require := require.New(t)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	repo := t.TempDir()

	calls := 0
	require.NoError(mgr.withRepoLock(t.Context(), repo, func() error {
		calls++
		return nil
	}))
	require.Equal(1, calls)

	again, err := mgr.locks.Acquire(t.Context(), repo)
	require.NoError(err)
	require.NoError(again.Unlock())
}

func TestManagerWithRepoLockReleaseOnError(t *testing.T) {
	require := require.New(t)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	repo := t.TempDir()

	sentinel := errors.New("inner failed")
	err := mgr.withRepoLock(t.Context(), repo, func() error {
		return sentinel
	})
	require.ErrorIs(err, sentinel)

	again, err := mgr.locks.Acquire(t.Context(), repo)
	require.NoError(err)
	require.NoError(again.Unlock())
}

func TestManagerAddWorktreeAcquiresRepoLock(t *testing.T) {
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	configureSameRepoPRRefs(t, cloneDir, "feature/lock-probe", 7)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	// Hold the per-repo lock from outside addWorktree; it must wait.
	held, err := mgr.locks.Acquire(t.Context(), cloneDir)
	require.NoError(err)

	ws := &Workspace{
		ID:           "ws-add-worktree-lock",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   7,
		GitHeadRef:   "feature/lock-probe",
		WorktreePath: filepath.Join(t.TempDir(), "wt"),
	}
	done := make(chan error, 1)
	go func() {
		_, err := mgr.addWorktree(
			t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
		)
		done <- err
	}()

	select {
	case <-done:
		require.FailNow("addWorktree completed while the per-repo lock was held")
	case <-time.After(80 * time.Millisecond):
	}

	require.NoError(held.Unlock())
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(5 * time.Second):
		require.FailNow("addWorktree did not finish after lock release")
	}
}

func TestManagerAddWorktreeRechecksOccupiedPathAfterWaitingForLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const (
		preferredBranch = "feature/occupied-after-preflight"
		prNumber        = 73
	)
	configureSameRepoPRRefs(t, cloneDir, preferredBranch, prNumber)
	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	ws := &Workspace{
		ID:           "ws-add-worktree-occupied-after-preflight",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   prNumber,
		GitHeadRef:   preferredBranch,
		WorktreePath: filepath.Join(t.TempDir(), "wt"),
	}

	held, err := mgr.locks.Acquire(t.Context(), cloneDir)
	require.NoError(err)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.addWorktree(
			t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws, workspaceGitFetchOptions{},
		)
		done <- err
	}()

	select {
	case <-done:
		require.FailNow("addWorktree completed while the per-repo lock was held")
	case <-time.After(80 * time.Millisecond):
	}
	require.NoError(os.MkdirAll(ws.WorktreePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "keep.txt"), []byte("preserve me\n"), 0o644,
	))
	require.NoError(held.Unlock())

	select {
	case err := <-done:
		require.Error(err)
	case <-time.After(5 * time.Second):
		require.FailNow("addWorktree did not finish after lock release")
	}
	contents, err := os.ReadFile(filepath.Join(ws.WorktreePath, "keep.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))
	for _, branch := range []string{
		preferredBranch, syntheticPRWorktreeBranch(prNumber),
	} {
		exists, branchErr := localBranchExists(t.Context(), cloneDir, branch)
		require.NoError(branchErr)
		assert.False(exists, "addWorktree must not leak branch %q", branch)
	}
}

func TestAddPreferredWorktreeRemovesBranchCreatedByFailedAdd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const (
		preferredBranch = "contributor/occupied-path"
		prNumber        = 74
	)
	configureForkPRRefs(t, cloneDir, preferredBranch, prNumber)
	headRepo := "https://github.com/contributor/widget.git"
	ws := &Workspace{
		ID:           "ws-failed-preferred-add",
		Platform:     "github",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   prNumber,
		GitHeadRef:   preferredBranch,
		MRHeadRepo:   &headRepo,
		WorktreePath: filepath.Join(t.TempDir(), "occupied"),
	}
	require.NoError(os.MkdirAll(ws.WorktreePath, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(ws.WorktreePath, "keep.txt"), []byte("preserve me\n"), 0o644,
	))
	mgr := newTestManager(t, openTestDB(t), t.TempDir())

	_, err := mgr.addPreferredWorktree(t.Context(), workspaceGitDir{path: cloneDir, remote: originRemoteName}, ws)

	require.Error(err)
	contents, err := os.ReadFile(filepath.Join(ws.WorktreePath, "keep.txt"))
	require.NoError(err)
	assert.Equal("preserve me\n", string(contents))
	exists, err := localBranchExists(t.Context(), cloneDir, preferredBranch)
	require.NoError(err)
	assert.False(exists, "a failed add must remove only the branch it created")
}

func TestFailedWorktreeAddPreservesConcurrentlyChangedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cloneDir := setupBareCloneForWorkspaceGitTest(t)
	const branch = "feature/concurrent-owner"
	divergentSHA := divergentCommitForWorkspaceGitTest(
		t, cloneDir,
		strings.TrimSpace(string(runWorkspaceTestGit(t, cloneDir, "rev-parse", "main"))),
	)
	realGit, err := exec.LookPath("git")
	require.NoError(err)
	fakeDir := t.TempDir()
	fakeGit := filepath.Join(fakeDir, "git")
	require.NoError(os.WriteFile(fakeGit, []byte(`#!/bin/sh
set -eu
case " $* " in
  *" worktree add "*)
    "$KENN_FORGE_TEST_REAL_GIT" --git-dir="$KENN_FORGE_TEST_GIT_DIR" \
      update-ref "$KENN_FORGE_TEST_BRANCH_REF" "$KENN_FORGE_TEST_BRANCH_SHA"
    exit 128
    ;;
esac
exec "$KENN_FORGE_TEST_REAL_GIT" "$@"
`), 0o755))
	t.Setenv("KENN_FORGE_TEST_REAL_GIT", realGit)
	t.Setenv("KENN_FORGE_TEST_GIT_DIR", cloneDir)
	t.Setenv("KENN_FORGE_TEST_BRANCH_REF", "refs/heads/"+branch)
	t.Setenv("KENN_FORGE_TEST_BRANCH_SHA", divergentSHA)
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = runGitWorktreeAddCreatingBranch(
		t.Context(), cloneDir, filepath.Join(t.TempDir(), "worktree"), branch, "main",
	)
	require.Error(err)
	branchSHA, exists, err := gitRefSHA(
		t.Context(), cloneDir, "refs/heads/"+branch,
	)
	require.NoError(err)
	assert.True(exists)
	assert.Equal(divergentSHA, branchSHA)
}

func TestManagerCleanupForDeleteAcquiresRepoLock(t *testing.T) {
	require := require.New(t)

	host, owner, name := "github.com", "acme", "widget"
	baseDir := t.TempDir()
	cloneDir := filepath.Join(baseDir, host, owner, name+".git")
	require.NoError(os.MkdirAll(filepath.Dir(cloneDir), 0o755))
	work := filepath.Join(t.TempDir(), "source")
	runWorkspaceTestGit(t, baseDir, "init", "--initial-branch=main", work)
	runWorkspaceTestGit(t, work, "config", "user.email", "test@test.com")
	runWorkspaceTestGit(t, work, "config", "user.name", "Test")
	require.NoError(os.WriteFile(
		filepath.Join(work, "base.txt"), []byte("base\n"), 0o644,
	))
	runWorkspaceTestGit(t, work, "add", ".")
	runWorkspaceTestGit(t, work, "commit", "-m", "base commit")
	runWorkspaceTestGit(
		t, baseDir, "clone", "--bare", work, cloneDir,
	)

	mgr := newTestManager(t, openTestDB(t), t.TempDir())
	mgr.SetClones(gitclone.New(baseDir, nil))
	worktreePath := filepath.Join(t.TempDir(), "missing-wt")
	runWorkspaceTestGit(
		t, cloneDir, "worktree", "add", worktreePath, "HEAD",
	)
	require.NoError(os.RemoveAll(worktreePath))

	ws := &Workspace{
		ID:           "ws-cleanup-lock",
		PlatformHost: host,
		RepoOwner:    owner,
		RepoName:     name,
		WorktreePath: worktreePath,
	}

	held, err := mgr.locks.Acquire(t.Context(), cloneDir)
	require.NoError(err)
	done := make(chan error, 1)
	go func() { done <- mgr.cleanupWorkspaceArtifactsForDelete(t.Context(), ws) }()

	select {
	case <-done:
		require.FailNow("cleanupWorkspaceArtifactsForDelete proceeded under held lock")
	case <-time.After(80 * time.Millisecond):
	}
	require.NoError(held.Unlock())
	select {
	case err := <-done:
		require.NoError(err)
	case <-time.After(5 * time.Second):
		require.FailNow("cleanupWorkspaceArtifactsForDelete did not finish after release")
	}
}

// TestSetupFailsClosedWhenRepositoryRouteReused verifies that setup — the
// chokepoint for every code-fetching path, including retries — refuses to
// operate when a workspace's stable repository no longer owns its route.
func TestSetupFailsClosedWhenRepositoryRouteReused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	observedAt := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	_, _, err := d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "provider-old",
		Owner:          "acme", Name: "widget",
	}, observedAt)
	require.NoError(err)
	ws := &Workspace{
		ID: "ws-route-reuse", Platform: "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 7,
		GitHeadRef:   "feature/reuse",
		WorktreePath: filepath.Join(t.TempDir(), "ws-route-reuse"),
		TmuxSession:  "ws-route-reuse",
		Status:       "creating",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))
	issuedAt := time.Now().UTC()
	require.NoError(d.PutWorkspaceLaunchSpec(t.Context(), ws.ID, WorkspaceLaunchSpec{
		Version: WorkspaceLaunchSpecVersion,
		Repository: WorkspaceLaunchRepository{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "provider-old", Owner: "acme", Name: "widget",
			CloneURL: "https://github.com/acme/widget.git", DefaultBranch: "main",
		},
		ItemType: ws.ItemType, ItemNumber: ws.ItemNumber,
		ItemKey: "7", GitHeadRef: ws.GitHeadRef,
		Pull: &WorkspaceLaunchPull{
			HeadBranch: ws.GitHeadRef, HeadRepoKind: "same_repo", SnapshotRevision: 1,
		},
		SourceVisible: true, IssuedAt: issuedAt,
		SourceVisibleUntil: issuedAt.Add(WorkspaceLaunchSpecVisibilityLease),
	}))
	_, _, err = d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "provider-new",
		Owner:          "acme", Name: "widget",
	}, observedAt.Add(time.Hour))
	require.NoError(err)

	mgr := newTestManager(t, d, t.TempDir())
	err = mgr.Setup(t.Context(), ws)
	require.ErrorContains(err, "repository identity changed")
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
}

func TestSetupRemovesManagedCloneWhenRepositoryRouteChangesDuringFetch(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	observedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var platformHost string
	platformHost, cloneURL := setupRouteChangingCloneRemoteForWorkspaceTest(
		t, func() error {
			_, _, err := database.ReconcileRepositoryObservation(
				context.Background(), db.RepoIdentity{
					Platform: "github", PlatformHost: platformHost,
					PlatformRepoID: "provider-replacement",
					Owner:          "acme", Name: "widget",
				}, observedAt.Add(time.Hour),
			)
			return err
		},
	)
	entry, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: platformHost,
			PlatformRepoID: "provider-original",
			Owner:          "acme", Name: "widget",
		}, observedAt,
	)
	require.NoError(err)
	require.NotNil(entry)
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), entry.Repository.ID, db.RepoProviderMetadata{
			CloneURL: cloneURL, DefaultBranch: "main",
		},
	))
	workspace := &Workspace{
		ID: "ws-route-change-during-fetch", Platform: "github",
		PlatformHost: platformHost, RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypeAdHoc, ItemKey: "spike/route-change",
		GitHeadRef: "spike/route-change", WorkspaceBranch: "spike/route-change",
		WorktreePath: filepath.Join(t.TempDir(), "worktree"),
		TmuxSession:  "forge-ws-route-change-during-fetch", Status: "creating",
	}
	require.NoError(database.InsertWorkspace(t.Context(), workspace))
	require.Equal(entry.Repository.ID, workspace.RepoID)

	clones := gitclone.New(t.TempDir(), nil)
	manager := NewManager(database, t.TempDir())
	manager.SetClones(clones)
	cloneCtx := gitclone.WithRepositoryIdentity(t.Context(), "provider-original")
	cloneDir, err := clones.ClonePathForContext(
		cloneCtx, "github", platformHost, "acme", "widget",
	)
	require.NoError(err)

	err = manager.Setup(t.Context(), workspace)
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	require.NoDirExists(cloneDir)
}

func TestCreateIssueRemovesManagedCloneWhenRepositoryRouteChangesDuringFetch(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	observedAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var platformHost string
	platformHost, cloneURL := setupRouteChangingCloneRemoteForWorkspaceTest(
		t, func() error {
			_, _, err := database.ReconcileRepositoryObservation(
				context.Background(), db.RepoIdentity{
					Platform: "github", PlatformHost: platformHost,
					PlatformRepoID: "provider-replacement",
					Owner:          "acme", Name: "widget",
				}, observedAt.Add(time.Hour),
			)
			return err
		},
	)
	entry, _, err := database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: platformHost,
			PlatformRepoID: "provider-original",
			Owner:          "acme", Name: "widget",
		}, observedAt,
	)
	require.NoError(err)
	require.NotNil(entry)
	require.NoError(database.UpdateRepoProviderMetadata(
		t.Context(), entry.Repository.ID, db.RepoProviderMetadata{
			CloneURL: cloneURL, DefaultBranch: "main",
		},
	))
	seedIssue(t, database, entry.Repository.ID, 7, "Route changes during clone")

	clones := gitclone.New(t.TempDir(), nil)
	manager := newTestManager(t, database, t.TempDir())
	manager.SetClones(clones)
	cloneCtx := gitclone.WithRepositoryIdentity(t.Context(), "provider-original")
	cloneDir, err := clones.ClonePathForContext(
		cloneCtx, "github", platformHost, "acme", "widget",
	)
	require.NoError(err)

	_, err = manager.CreateIssue(
		t.Context(), platformHost, "acme", "widget", 7,
		CreateIssueOptions{Provider: "github"},
	)
	require.Error(err)
	require.NoDirExists(cloneDir)
}

func TestSetupFailsBeforeGitWhenSourceItemWasRemovedUpstream(t *testing.T) {
	for _, itemType := range []string{
		db.WorkspaceItemTypePullRequest,
		db.WorkspaceItemTypeIssue,
	} {
		t.Run(string(itemType), func(t *testing.T) {
			require := require.New(t)
			d := openTestDB(t)
			repoID := seedRepo(t, d, "github.com", "acme", "widget")
			if itemType == db.WorkspaceItemTypePullRequest {
				seedMR(t, d, repoID, 7, "feature/removed")
			} else {
				seedIssue(t, d, repoID, 7, "Removed issue")
			}

			mgr := newTestManager(t, d, t.TempDir())
			var resolverCalls atomic.Int32
			var ws *Workspace
			var err error
			if itemType == db.WorkspaceItemTypePullRequest {
				ws, err = mgr.Create(
					t.Context(), "github", "github.com", "acme", "widget", 7,
				)
			} else {
				ws, err = mgr.CreateIssue(
					t.Context(), "github.com", "acme", "widget", 7,
					CreateIssueOptions{Provider: "github"},
				)
			}
			require.NoError(err)
			spec, specErr := d.GetWorkspaceLaunchSpec(t.Context(), ws.ID)
			require.NoError(specErr)
			require.NotNil(spec)
			require.NotNil(ws)
			mgr.SetWorktreeBasePathResolver(func(
				context.Context, WorktreeBaseRepository,
			) (string, bool, error) {
				resolverCalls.Add(1)
				return "", false, errors.New("git setup must not start")
			})

			archiveType := db.ArchiveItemTypeIssue
			if itemType == db.WorkspaceItemTypePullRequest {
				archiveType = db.ArchiveItemTypeMergeRequest
			}
			now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
			_, err = d.WriteDB().ExecContext(t.Context(), `
				INSERT INTO forge_archive_items (
					repo_id, item_type, item_number, provider_item_id,
					provider_created_at, provider_updated_at, lifecycle_state
				) VALUES (?, ?, ?, ?, ?, ?, 'removed_upstream')`,
				repoID, archiveType, 7, string(archiveType)+"-7", now, now,
			)
			require.NoError(err)
			refreshAt := spec.SourceVisibleUntil.Add(time.Second)
			mgr.SetNow(func() time.Time { return refreshAt })
			mgr.SetLaunchSpecResolver(databaseLaunchSpecResolver{
				db: d, now: func() time.Time { return refreshAt },
			})

			err = mgr.Setup(t.Context(), ws)

			require.ErrorIs(err, ErrLaunchSpecSourceHidden)
			require.Zero(resolverCalls.Load(),
				"workspace setup must stop before resolving a Git base")
			require.NoDirExists(ws.WorktreePath)
			stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
			require.NoError(getErr)
			require.NotNil(stored)
			require.Equal("error", stored.Status)
		})
	}
}

func TestRefreshWorkspaceHeadRepoSnapshotSurvivesQueuedReconciliation(t *testing.T) {
	require := require.New(t)
	d := openTestDB(t)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	seedMRWithHeadRepo(
		t, d, repoID, 42, "feature/thing", "https://github.com/acme/widget.git",
	)
	ws := &Workspace{
		ID:           "ws-refresh-queued-writer",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypePullRequest,
		ItemNumber:   42,
		GitHeadRef:   "feature/thing",
		WorktreePath: t.TempDir(),
		Status:       "ready",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))

	mgr := NewManager(d, t.TempDir())
	writerQueued := make(chan struct{})
	restoreHook := d.SetBeforeRepositoryReconciliationWriteLockForTest(func() {
		close(writerQueued)
	})
	defer restoreHook()
	writerDone := make(chan error, 1)
	var interleaved bool
	mgr.beforeHeadRepoSnapshotRepoLookup = func() {
		if interleaved {
			return
		}
		interleaved = true
		go func() {
			_, _, err := d.ReconcileRepositoryObservation(
				context.Background(), db.RepoIdentity{
					Platform: "github", PlatformHost: "github.com",
					PlatformRepoID: "repo-acme-other",
					Owner:          "acme", Name: "other",
					RepoPath: "acme/other",
				}, time.Now().UTC(),
			)
			writerDone <- err
		}()
		<-writerQueued
	}

	refreshDone := make(chan error, 1)
	go func() {
		_, err := mgr.RefreshWorkspaceHeadRepoSnapshot(context.Background(), ws)
		refreshDone <- err
	}()
	select {
	case err := <-refreshDone:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.Fail("head-repo refresh deadlocked behind a queued reconciliation writer")
	}
	select {
	case err := <-writerDone:
		require.NoError(err)
	case <-time.After(10 * time.Second):
		require.Fail("reconciliation writer never completed")
	}
}

func TestSyncWorkspaceBaseBranchSurvivesQueuedReconciliationWriter(t *testing.T) {
	if os.Getenv("KENN_FORGE_TEST_SYNC_BASE_BRANCH_QUEUED_WRITER") != "1" {
		preparedDBPath := filepath.Join(t.TempDir(), "prepared.db")
		preparedDB := dbtest.OpenAt(t, preparedDBPath)
		require.NoError(t, preparedDB.Close())
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := procutil.CommandContext(
			ctx, os.Args[0],
			"-test.run=^TestSyncWorkspaceBaseBranchSurvivesQueuedReconciliationWriter$",
		)
		cmd.Env = append(
			os.Environ(),
			"KENN_FORGE_TEST_SYNC_BASE_BRANCH_QUEUED_WRITER=1",
			"KENN_FORGE_TEST_SYNC_BASE_BRANCH_DB="+preparedDBPath,
		)
		output, err := cmd.CombinedOutput()
		require.NoError(t, ctx.Err(),
			"workspace base-branch sync deadlocked behind a queued reconciliation writer: %s", output,
		)
		require.NoError(t, err, string(output))
		return
	}

	require := require.New(t)
	preparedDBPath := os.Getenv("KENN_FORGE_TEST_SYNC_BASE_BRANCH_DB")
	require.NotEmpty(preparedDBPath)
	d := dbtest.OpenPreparedAt(t, preparedDBPath)
	repoID := seedRepo(t, d, "github.com", "acme", "widget")
	ws := &Workspace{
		ID: "ws-verify-queued-writer", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", RepoID: repoID,
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
		WorktreePath: t.TempDir(), Status: "ready",
	}
	require.NoError(d.InsertWorkspace(t.Context(), ws))

	d.ReadDB().SetMaxOpenConns(1)
	readConn, err := d.ReadDB().Conn(t.Context())
	require.NoError(err)
	syncDone := make(chan error, 1)
	mgr := NewManager(d, t.TempDir())
	go func() {
		syncDone <- mgr.syncWorkspaceBaseBranch(
			context.Background(), ws.WorktreePath, originRemoteName, ws,
		)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for d.ReadDB().Stats().WaitCount == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	require.Positive(d.ReadDB().Stats().WaitCount, "base-branch sync never reached its repository read")

	writerQueued := make(chan struct{})
	restoreHook := d.SetBeforeRepositoryReconciliationWriteLockForTest(func() {
		close(writerQueued)
	})
	defer restoreHook()
	writerDone := make(chan error, 1)
	go func() {
		_, _, err := d.ReconcileRepositoryObservation(
			context.Background(), db.RepoIdentity{
				Platform: "github", PlatformHost: "github.com",
				PlatformRepoID: "repo-acme-other", Owner: "acme", Name: "other",
				RepoPath: "acme/other",
			}, time.Now().UTC(),
		)
		writerDone <- err
	}()
	<-writerQueued
	require.NoError(readConn.Close())
	require.NoError(<-syncDone)
	require.NoError(<-writerDone)
}
