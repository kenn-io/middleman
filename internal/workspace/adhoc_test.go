package workspace

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
)

func TestCreateAdHocGeneratesBranchWhenUnnamed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{},
	)
	require.NoError(err)
	require.NotNil(ws)

	assert.Equal(db.WorkspaceItemTypeAdHoc, ws.ItemType)
	assert.Equal(0, ws.ItemNumber)
	assert.True(strings.HasPrefix(ws.GitHeadRef, "kenn-forge/work-"),
		"generated branch %q should carry the work prefix", ws.GitHeadRef)
	assert.Equal(ws.GitHeadRef, ws.WorkspaceBranch)
	assert.Equal(db.AdHocWorkspaceItemKey(ws.GitHeadRef), ws.ItemKey)
	assert.Contains(ws.WorktreePath, filepath.Join("acme", "widget"))
}

func TestCreateAdHocUsesRequestedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "  spike/rate-limits  "},
	)
	require.NoError(err)
	require.NotNil(ws)

	assert.Equal("spike/rate-limits", ws.GitHeadRef)
	assert.Equal("adhoc:spike/rate-limits", ws.ItemKey)
	assert.Contains(filepath.Base(ws.WorktreePath), "work-spike-rate-limits-")
}

func TestCreateAdHocRejectsInvalidBranch(t *testing.T) {
	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())

	_, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "bad branch..name"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid branch name")
}

func TestCreateAdHocRejectsUntrackedRepo(t *testing.T) {
	d := openTestDB(t)
	mgr := NewManager(d, t.TempDir())

	_, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/thing"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not tracked")
}

func TestCreateAdHocSameBranchTwiceIsDuplicate(t *testing.T) {
	require := require.New(t)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())

	_, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/thing"},
	)
	require.NoError(err)

	_, err = mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/thing"},
	)
	require.Error(err)
	require.ErrorIs(err, ErrWorkspaceDuplicate)
}

// Two ad-hoc workspaces whose branch names slugify identically must not share
// a worktree directory.
func TestCreateAdHocDistinctBranchesGetDistinctWorktrees(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())

	first, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/rate-limits"},
	)
	require.NoError(err)
	second, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: "spike-rate-limits"},
	)
	require.NoError(err)

	assert.NotEqual(first.ItemKey, second.ItemKey)
	assert.NotEqual(first.WorktreePath, second.WorktreePath)
}

func TestCreateAdHocExistingLocalBranchIsUniquified(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "spike/thing"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/other")
	runWorkspaceTestGit(t, localRepo, "branch", branch)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: branch},
	)

	require.NoError(err)
	require.NotNil(ws)
	assert.Regexp(`^spike/thing-[0-9a-f]{4}$`, ws.GitHeadRef)
	assert.Equal(ws.GitHeadRef, ws.WorkspaceBranch)
	assert.Equal(db.AdHocWorkspaceItemKey(ws.GitHeadRef), ws.ItemKey)
}

func TestNextAvailableAdHocBranchNameAvoidsRefNamespaceConflicts(t *testing.T) {
	const workspaceID = "0011223344556677"
	firstHash := adHocBranchHash(workspaceID, 0)
	secondHash := adHocBranchHash(workspaceID, 1)
	tests := []struct {
		name      string
		existing  []string
		requested string
		want      string
		wantNext  int
	}{
		{
			name:      "descendant",
			existing:  []string{"docs/guide-refresh"},
			requested: "docs",
			want:      "docs-" + firstHash,
			wantNext:  1,
		},
		{
			name:      "hash collision",
			existing:  []string{"docs/guide-refresh", "docs-" + firstHash},
			requested: "docs",
			want:      "docs-" + secondHash,
			wantNext:  2,
		},
		{
			name:      "ancestor",
			existing:  []string{"docs"},
			requested: "docs/guide-refresh",
			want:      "docs-" + firstHash + "/guide-refresh",
			wantNext:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/other")
			for _, branch := range tt.existing {
				runWorkspaceTestGit(t, localRepo, "branch", branch)
			}

			got, nextAttempt, err := nextAvailableAdHocBranchName(
				t.Context(), localRepo, tt.requested, workspaceID, 0,
			)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantNext, nextAttempt)
		})
	}
}

func TestPersistAdHocWorkspaceRetriesReservedHashedBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const (
		workspaceID = "0011223344556677"
		requested   = "docs"
	)
	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/other")
	runWorkspaceTestGit(t, localRepo, "branch", "docs/guide-refresh")
	mgr := NewManager(d, t.TempDir())

	firstBranch, nextAttempt, err := nextAvailableAdHocBranchName(
		t.Context(), localRepo, requested, workspaceID, 0,
	)
	require.NoError(err)
	require.NoError(d.InsertWorkspace(t.Context(), &Workspace{
		ID:              "reserved-workspace",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeAdHoc,
		ItemKey:         db.AdHocWorkspaceItemKey(firstBranch),
		GitHeadRef:      firstBranch,
		WorkspaceBranch: firstBranch,
		WorktreePath:    filepath.Join(t.TempDir(), "reserved-worktree"),
		TmuxSession:     "forge-reserved-workspace",
		TerminalBackend: "tmux",
		Status:          "creating",
	}))

	ws := &Workspace{
		ID:              workspaceID,
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeAdHoc,
		TmuxSession:     "forge-" + workspaceID,
		TerminalBackend: "tmux",
		Status:          "creating",
	}
	mgr.setAdHocWorkspaceIdentity(ws, firstBranch, firstBranch)

	require.NoError(mgr.persistAdHocWorkspace(
		t.Context(), ws, localRepo, requested, nextAttempt,
	))
	assert.NotEqual(firstBranch, ws.GitHeadRef)
	assert.Regexp(`^docs-[0-9a-f]{4}$`, ws.GitHeadRef)
	assert.Equal(ws.GitHeadRef, ws.WorkspaceBranch)
	assert.Equal(db.AdHocWorkspaceItemKey(ws.GitHeadRef), ws.ItemKey)

	stored, err := d.GetWorkspace(t.Context(), workspaceID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(ws.GitHeadRef, stored.GitHeadRef)
}

func TestCreateAdHocReuseExistingLocalBranch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const branch = "spike/thing"
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/other")
	runWorkspaceTestGit(t, localRepo, "branch", branch)

	d := openTestDB(t)
	seedRepo(t, d, "github.com", "acme", "widget")
	mgr := NewManager(d, t.TempDir())
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", "github.com", "acme", "widget",
		CreateAdHocOptions{BranchName: branch, ReuseExistingBranch: true},
	)
	require.NoError(err)
	require.NotNil(ws)

	// An empty workspace branch means kenn-forge did not create the branch, so
	// rollback and delete leave the user's pre-existing branch alone.
	assert.Empty(ws.WorkspaceBranch)
	assert.Equal(branch, ws.GitHeadRef)
}

func TestSetupAdHocWorkspaceBranchesFromOriginHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	seedRepo(t, d, platformHost, "acme", "widget")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/thing"},
	)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Equal("spike/thing", got.WorkspaceBranch)

	head := strings.TrimSpace(string(runWorkspaceTestGit(
		t, got.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD",
	)))
	assert.Equal("spike/thing", head)
	originHead := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "refs/remotes/origin/HEAD",
	)))
	worktreeHead := strings.TrimSpace(string(runWorkspaceTestGit(
		t, got.WorktreePath, "rev-parse", "HEAD",
	)))
	assert.Equal(originHead, worktreeHead)
}

func TestConfigureFallbackBranchUpstreamIgnoresAdHocWorkspace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const (
		headBranch     = "spike/thing"
		fallbackBranch = "kenn-forge/work-abcd"
	)
	localRepo := setupLocalWorktreeBaseForWorkspaceGitTest(t, "feature/other")
	headSHA := strings.TrimSpace(string(runWorkspaceTestGit(
		t, localRepo, "rev-parse", "HEAD",
	)))
	runWorkspaceTestGit(
		t, localRepo, "update-ref", "refs/remotes/origin/"+headBranch, headSHA,
	)
	runWorkspaceTestGit(t, localRepo, "branch", fallbackBranch, headSHA)
	worktreePath := filepath.Join(t.TempDir(), "worktree")
	runWorkspaceTestGit(
		t, localRepo, "worktree", "add", worktreePath, fallbackBranch,
	)

	err := configureFallbackBranchUpstream(
		t.Context(), workspaceGitDir{path: localRepo, remote: originRemoteName},
		&Workspace{
			ItemType:     db.WorkspaceItemTypeAdHoc,
			GitHeadRef:   headBranch,
			WorktreePath: worktreePath,
		},
		fallbackBranch,
	)
	require.NoError(err)

	_, err = gitConfigValue(
		t.Context(), worktreePath, "branch."+fallbackBranch+".remote",
	)
	assert.Error(err)
}

func TestSetupAdHocWorkspaceUsesHashedBranchForPrefixConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	runWorkspaceTestGit(t, localRepo, "branch", "docs/guide-refresh")
	seedRepo(t, d, platformHost, "acme", "widget")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "docs"},
	)
	require.NoError(err)
	require.NoError(mgr.Setup(t.Context(), ws))

	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("ready", got.Status)
	assert.Regexp(`^docs-[0-9a-f]{4}$`, got.GitHeadRef)
	assert.Equal(got.GitHeadRef, got.WorkspaceBranch)
	assert.Equal(db.AdHocWorkspaceItemKey(got.GitHeadRef), got.ItemKey)

	head := strings.TrimSpace(string(runWorkspaceTestGit(
		t, got.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD",
	)))
	assert.Equal(got.GitHeadRef, head)
}

func TestSetupAdHocWorkspaceLateBranchConflictFailsWithoutChangingIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	seedRepo(t, d, platformHost, "acme", "widget")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "docs"},
	)
	require.NoError(err)
	runWorkspaceTestGit(t, localRepo, "branch", "docs/guide-refresh")

	require.Error(mgr.Setup(t.Context(), ws))
	got, err := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal("error", got.Status)
	assert.Equal("docs", got.GitHeadRef)
	assert.Equal("docs", got.WorkspaceBranch)
	assert.Equal(db.AdHocWorkspaceItemKey("docs"), got.ItemKey)
}

func TestAgentContextForAdHocWorkspace(t *testing.T) {
	assert := assert.New(t)

	summary := WorkspaceSummary{
		ID:              "ws-adhoc",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeAdHoc,
		GitHeadRef:      "spike/thing",
		WorkspaceBranch: "spike/thing"}

	rendered := RenderAgentContext(BuildAgentContext(summary))

	assert.Contains(rendered, "Source kind: "+AgentSourceKindAdHoc)
	assert.Contains(rendered, "Working branch: spike/thing")
	assert.Contains(rendered, "No linked pull request, issue, or task")
	assert.NotContains(rendered, "- PR:")
	assert.NotContains(rendered, "- Issue:")
}

func TestAgentContextForAdHocWorkspaceWithDetectedPR(t *testing.T) {
	assert := assert.New(t)

	prNumber := 41
	summary := WorkspaceSummary{
		ID:                  "ws-adhoc",
		Platform:            "github",
		PlatformHost:        "github.com",
		RepoOwner:           "acme",
		RepoName:            "widget",
		ItemType:            db.WorkspaceItemTypeAdHoc,
		GitHeadRef:          "spike/thing",
		WorkspaceBranch:     "spike/thing",
		AssociatedPRNumber:  &prNumber,
		AssociatedPRVisible: true}

	rendered := RenderAgentContext(BuildAgentContext(summary))

	assert.Contains(rendered, "Associated PR: #41")
	assert.NotContains(rendered, "No linked pull request")
}

// Before setup materializes the worktree the workspace branch is the unknown
// sentinel, which must never leak into the generated context.
func TestAgentContextForAdHocWorkspaceBeforeSetup(t *testing.T) {
	assert := assert.New(t)

	summary := WorkspaceSummary{
		ID:              "ws-adhoc",
		Platform:        "github",
		PlatformHost:    "github.com",
		RepoOwner:       "acme",
		RepoName:        "widget",
		ItemType:        db.WorkspaceItemTypeAdHoc,
		GitHeadRef:      "spike/thing",
		WorkspaceBranch: workspaceBranchUnknown}

	rendered := RenderAgentContext(BuildAgentContext(summary))

	assert.Contains(rendered, "Working branch: spike/thing")
	assert.NotContains(rendered, workspaceBranchUnknown)
}

// TestSetupFailsClosedWhenRouteReplacedMidSetup interleaves a route
// replacement between setup's initial collision check and its final
// persist, verifying the re-validation catches replacements that land
// while the clone and worktree work is in flight.
func TestSetupFailsClosedWhenRouteReplacedMidSetup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	seedRepo(t, d, platformHost, "acme", "widget")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/replaced"},
	)
	require.NoError(err)

	mgr.beforeSetupRouteRevalidation = func() {
		_, _, replaceErr := d.ReconcileRepositoryObservation(
			t.Context(), db.RepoIdentity{
				Platform:       "github",
				PlatformHost:   platformHost,
				PlatformRepoID: "repo-acme-widget-replacement",
				Owner:          "acme",
				Name:           "widget",
			}, time.Now().UTC(),
		)
		require.NoError(replaceErr)
	}

	err = mgr.Setup(t.Context(), ws)
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
}

func TestSetupFailsClosedWhenRepositoryRenamedAndRouteReplacedMidSetup(t *testing.T) {
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	seedRepo(t, d, platformHost, "acme", "widget")

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/renamed-and-replaced"},
	)
	require.NoError(err)

	mgr.beforeSetupRouteRevalidation = func() {
		_, _, renameErr := d.ReconcileRepositoryObservation(
			t.Context(), db.RepoIdentity{
				Platform: "github", PlatformHost: platformHost,
				PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "renamed",
			}, time.Now().UTC(),
		)
		require.NoError(renameErr)
		_, _, replaceErr := d.ReconcileRepositoryObservation(
			t.Context(), db.RepoIdentity{
				Platform: "github", PlatformHost: platformHost,
				PlatformRepoID: "repo-acme-widget-replacement", Owner: "acme", Name: "widget",
			}, time.Now().UTC().Add(time.Second),
		)
		require.NoError(replaceErr)
	}

	err = mgr.Setup(t.Context(), ws)
	require.ErrorIs(err, db.ErrRepositoryRouteFenceChanged)
	stored, getErr := d.GetWorkspace(t.Context(), ws.ID)
	require.NoError(getErr)
	require.NotNil(stored)
	require.Equal("error", stored.Status)
}

func TestSetupUsesCurrentRepositoryAfterRouteReuse(t *testing.T) {
	require := require.New(t)

	d := openTestDB(t)
	localRepo, _, platformHost := setupHTTPWorktreeBaseForWorkspaceGitTest(
		t, "feature/other",
	)
	observedAt := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	_, _, err := d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   platformHost,
		PlatformRepoID: "repo-original",
		Owner:          "acme",
		Name:           "widget",
	}, observedAt)
	require.NoError(err)
	current, _, err := d.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   platformHost,
		PlatformRepoID: "repo-current",
		Owner:          "acme",
		Name:           "widget",
	}, observedAt.Add(time.Hour))
	require.NoError(err)
	require.NotNil(current)

	tmuxScript, _ := writeRecorderScript(t)
	mgr := NewManager(d, t.TempDir())
	mgr.SetTmuxCommand([]string{tmuxScript})
	mgr.SetWorktreeBasePathResolver(staticBaseResolver(localRepo))

	ws, err := mgr.CreateAdHoc(
		t.Context(), "github", platformHost, "acme", "widget",
		CreateAdHocOptions{BranchName: "spike/current-repository"},
	)
	require.NoError(err)
	require.Equal(current.Repository.ID, ws.RepoID)

	require.NoError(mgr.Setup(t.Context(), ws))
	require.Equal("ready", ws.Status)
}
