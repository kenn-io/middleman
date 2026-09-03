package workspacetest

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func TestListWorkspacesIncludesKataMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := setupWorkspaceServerFixture(t, nil)
	client := fixture.client
	database := fixture.database
	ctx := t.Context()

	repo, err := database.GetRepoByID(ctx, fixture.repoID)
	require.NoError(err)
	require.NotNil(repo)

	metadata := db.WorkspaceKataMetadata{
		DaemonID:    "desktop",
		ProjectUID:  "project-kata",
		ProjectName: "Widget",
		IssueUID:    "issue-kata-1",
		ShortID:     "task-123",
		QualifiedID: "Kata#task-123",
		Title:       "Wire kata workspace sidebar",
	}
	itemKey := db.KataWorkspaceItemKey(metadata)
	require.NotEmpty(itemKey)
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID:           "ws-kata-list",
		Platform:     "github",
		PlatformHost: "github.com",
		RepoOwner:    "acme",
		RepoName:     "widget",
		ItemType:     db.WorkspaceItemTypeKataTask,
		ItemKey:      itemKey,
		GitHeadRef:   "kenn-forge/kata/task-123-abcd1234",
		WorktreePath: filepath.Join(t.TempDir(), "ws-kata-list"),
		TmuxSession:  "kenn-forge-ws-kata-list",
		Status:       "creating",
		CreatedAt:    time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
		KataMetadata: &metadata,
	}))

	// The workspace list UI reloads from GET /workspaces, so the kata owner
	// metadata it renders must survive the DB summary hydration on that path,
	// not just the create response.
	resp, err := client.HTTP.ListWorkspacesWithResponse(ctx)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode())
	require.NotNil(resp.JSON200)
	require.NotNil(resp.JSON200.Workspaces)

	var kata *generated.WorkspaceResponse
	for i := range resp.JSON200.Workspaces {
		if resp.JSON200.Workspaces[i].Id == "ws-kata-list" {
			kata = &resp.JSON200.Workspaces[i]
			break
		}
	}
	require.NotNil(kata)
	assert.Equal(db.WorkspaceItemTypeKataTask, kata.ItemType)
	assert.Equal(itemKey, kata.ItemKey)
	// item_number is always emitted and is 0 (ignored) for Kata workspaces.
	assert.Equal(int64(0), kata.ItemNumber)
	require.NotNil(kata.Kata)
	assert.Equal("desktop", kata.Kata.DaemonId)
	assert.Equal("issue-kata-1", kata.Kata.IssueUid)
	assert.Equal("project-kata", kata.Kata.ProjectUid)
	require.NotNil(kata.Kata.ProjectName)
	assert.Equal("Widget", *kata.Kata.ProjectName)
	require.NotNil(kata.Kata.ShortId)
	assert.Equal("task-123", *kata.Kata.ShortId)
	require.NotNil(kata.Kata.QualifiedId)
	assert.Equal("Kata#task-123", *kata.Kata.QualifiedId)
	require.NotNil(kata.Kata.Title)
	assert.Equal("Wire kata workspace sidebar", *kata.Kata.Title)
}

func TestWorkspaceRuntimeNaturalTmuxAgentExitForgetsStoredSessionE2E(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "fake-tmux")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		`if [ "$1" = "-u" ]; then shift; fi
case "$1" in
  has-session)
    echo "can't find session: $3" >&2
    exit 1
    ;;
  new-session|set-option|attach-session)
    exit 0
    ;;
esac
exit 0
`), 0o755))

	cfg := &config.Config{
		Agents: []config.Agent{{
			Key:     "helper",
			Label:   "Helper",
			Command: []string{"/bin/sh", "-lc", "exit 0"},
		}},
		Tmux: config.Tmux{Command: []string{tmuxPath}},
	}
	fixture := setupWorkspaceServerFixture(t, cfg)
	client := fixture.client
	database := fixture.database
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)

	launchResp, err := client.HTTP.LaunchWorkspaceRuntimeSessionWithResponse(
		ctx, ws.Id,
		generated.LaunchWorkspaceRuntimeSessionInputBody{
			TargetKey: "helper",
		},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, launchResp.StatusCode())
	require.NotNil(launchResp.JSON200)

	require.Eventually(func() bool {
		runtimeResp, runtimeErr := client.HTTP.GetWorkspaceRuntimeWithResponse(
			ctx, ws.Id,
		)
		if runtimeErr != nil ||
			runtimeResp.StatusCode() != http.StatusOK ||
			runtimeResp.JSON200 == nil ||
			runtimeResp.JSON200.Sessions == nil {
			return false
		}
		return len(runtimeResp.JSON200.Sessions) == 0
	}, 2*time.Second, 20*time.Millisecond)

	require.Eventually(func() bool {
		stored, storedErr := database.ListWorkspaceRuntimeTmuxSessions(ctx, ws.Id)
		return storedErr == nil && len(stored) == 0
	}, 2*time.Second, 20*time.Millisecond)
	assert.NotEmpty(launchResp.JSON200.Key)
}

func TestWorkspaceResponseUsesStoredRuntimeTmuxSessionsAfterRestartE2E(
	t *testing.T,
) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	tmuxPath := filepath.Join(dir, "fake-tmux")
	liveSessionsFile := filepath.Join(dir, "live-sessions")
	require.NoError(os.WriteFile(tmuxPath, []byte("#!/bin/sh\n"+
		"TMUX_LIVE_SESSIONS_FILE="+shellquote.Join(liveSessionsFile)+"\n"+
		`target=""
mode=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-t" ]; then target="$a"; fi
  if [ "$a" = "display-message" ]; then mode="display-message"; fi
  if [ "$a" = "capture-pane" ]; then mode="capture-pane"; fi
  if [ "$a" = "list-sessions" ]; then
    [ -f "$TMUX_LIVE_SESSIONS_FILE" ] && cat "$TMUX_LIVE_SESSIONS_FILE"
    exit 0
  fi
  prev="$a"
done
if [ "$mode" = "display-message" ]; then
  case "$target" in
    *-claude) printf '⠴ claude-activity\n' ;;
    *) printf 'idle\n' ;;
  esac
  exit 0
fi
if [ "$mode" = "capture-pane" ]; then
  printf 'stable\n'
  exit 0
fi
exit 0
	`), 0o755))
	cfg := &config.Config{Tmux: config.Tmux{Command: []string{tmuxPath}}}
	fixture := setupWorkspaceServerFixture(t, cfg)
	client := fixture.client
	database := fixture.database
	ctx := context.Background()
	ws := createReadyWorkspace(t, ctx, client)
	require.NotEmpty(ws.TmuxSession)
	require.NoError(os.WriteFile(liveSessionsFile, []byte(strings.Join([]string{
		ws.TmuxSession,
		ws.TmuxSession + "-codex",
		ws.TmuxSession + "-claude",
	}, "\n")+"\n"), 0o644))
	for _, targetKey := range []string{"codex", "claude"} {
		sessionKey, err := localruntime.NewSessionKey(ws.Id)
		require.NoError(err)
		require.NoError(database.UpsertWorkspaceRuntimeSession(
			ctx,
			&db.WorkspaceRuntimeSession{
				WorkspaceID: ws.Id,
				SessionKey:  sessionKey,
				TargetKey:   targetKey,
				Label:       targetKey,
				Kind:        string(localruntime.LaunchTargetAgent),
				Scope:       "session",
				TmuxSession: ws.TmuxSession + "-" + targetKey,
			},
		))
	}

	var listed *generated.WorkspaceResponse
	require.Eventually(func() bool {
		listResp, err := client.HTTP.ListWorkspacesWithResponse(ctx)
		require.NoError(err)
		if listResp.StatusCode() != http.StatusOK ||
			listResp.JSON200 == nil || listResp.JSON200.Workspaces == nil {
			return false
		}
		listed = nil
		for i := range listResp.JSON200.Workspaces {
			if listResp.JSON200.Workspaces[i].Id == ws.Id {
				listed = &listResp.JSON200.Workspaces[i]
				break
			}
		}
		return listed != nil && listed.TmuxWorking &&
			listed.TmuxActivitySource == workspaceapi.TmuxActivitySourceTitle &&
			listed.TmuxPaneTitle != nil
	}, 6*time.Second, 10*time.Millisecond)
	require.NotNil(listed)
	assert.True(listed.TmuxWorking)
	assert.Equal(workspaceapi.TmuxActivitySourceTitle, listed.TmuxActivitySource)
	require.NotNil(listed.TmuxPaneTitle)
	assert.Equal("⠴ claude-activity", *listed.TmuxPaneTitle)
}

func TestWorkspaceDiffCacheHitReturnsWhileGitCapacityIsHeldE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	restoreLimiter := procutil.SetDefaultLimiterForTest(
		procutil.NewLimiterWithAcquireTimeout(1, 500*time.Millisecond),
	)
	t.Cleanup(restoreLimiter)

	fixture := setupWorkspaceServerFixture(t, nil)
	ws := createReadyWorkspace(t, context.Background(), fixture.client)
	initial := requestWorkspaceDiff(t, fixture.server, ws.Id, "head")
	assert.False(initial.Stale)

	releaseHeld, err := procutil.TryAcquire(
		context.Background(), "test-held workspace diff capacity",
	)
	require.NoError(err)
	defer releaseHeld()

	started := time.Now()
	cached := requestWorkspaceDiff(t, fixture.server, ws.Id, "head")
	elapsed := time.Since(started)
	releaseHeld()

	assert.False(cached.Stale)
	assert.Less(elapsed, 200*time.Millisecond)
}
