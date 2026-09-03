package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	shellquote "github.com/kballard/go-shellquote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writeTmuxRecorder creates an executable fake-tmux script at a
// fresh temp path. The script appends NUL-delimited argv to
// record. For has-session it emits tmux's "can't find session"
// stderr and exits 1 (so EnsureTmux's isTmuxSessionAbsent check
// sees the canonical signal and proceeds to new-session); all
// other invocations exit 0. Returns the script path and the record
// path.
func writeTmuxRecorder(t *testing.T) (script, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	script = filepath.Join(dir, "fake-tmux")
	// Control paths are baked into the script and dynamic pane title /
	// output values are read from files derived from the record path:
	// tmux clients run with the non-secret allowlist environment, so
	// fixtures cannot smuggle state through custom env vars.
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`session_file="${TMUX_RECORD}.sessions"` + "\n" +
		`new_session=""` + "\n" +
		`prev=""` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$prev" = "-s" ]; then new_session="$a"; fi` + "\n" +
		`  if [ "$a" = "list-sessions" ]; then` + "\n" +
		`    [ -f "$session_file" ] && cat "$session_file"` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "display-message" ]; then` + "\n" +
		`    if [ -f "${TMUX_RECORD}.pane-title" ]; then` + "\n" +
		`      cat "${TMUX_RECORD}.pane-title"` + "\n" +
		`    else` + "\n" +
		`      printf '\n'` + "\n" +
		`    fi` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "capture-pane" ]; then` + "\n" +
		`    if [ -f "${TMUX_RECORD}.pane-output" ]; then` + "\n" +
		`      cat "${TMUX_RECORD}.pane-output"` + "\n" +
		`    else` + "\n" +
		`      printf '\n'` + "\n" +
		`    fi` + "\n" +
		`    exit 0` + "\n" +
		`  fi` + "\n" +
		`  prev="$a"` + "\n" +
		"done\n" +
		`if [ -n "$new_session" ]; then printf '%s\n' "$new_session" >> "$session_file"; fi` + "\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	return script, record
}

// setTmuxRecorderPaneTitle sets the value the fake tmux returns for
// display-message pane-title probes.
func setTmuxRecorderPaneTitle(t *testing.T, record, title string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		record+".pane-title", []byte(title+"\n"), 0o644,
	))
}

// setTmuxRecorderPaneOutput sets the value the fake tmux returns for
// capture-pane probes.
func setTmuxRecorderPaneOutput(t *testing.T, record, output string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		record+".pane-output", []byte(output+"\n"), 0o644,
	))
}

func readTmuxRecord(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	// Split on NUL. Each record is "<argc>\0<arg0>\0<arg1>\0...\0",
	// so a flushed stream always ends with a trailing \0 and Split
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
		if err != nil {
			// Trailing record is mid-write: argc isn't a valid
			// integer yet. Stop; the next poll will see the full
			// record once the recorder script flushes.
			break
		}
		if i+1+n > len(parts) {
			// argc is parsed but not all args are on disk yet.
			// Same treatment: defer to the next poll.
			break
		}
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

func argAfter(argv []string, flag string) (string, bool) {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// setupWrapperServer constructs a full server wired with a
// recording-script tmux command, a bare repo, and a seeded PR.
// Returns a generated API client pointed at the httptest server,
// the httptest baseURL (needed for WebSocket dialing), and the
// record-file path.
func setupWrapperServer(t *testing.T) (client *apiclient.Client, baseURL, record string) {
	t.Helper()
	script, record := writeTmuxRecorder(t)
	client, baseURL = setupWrapperServerWithScript(t, script)
	return client, baseURL, record
}

// setupWrapperServerWithScript is setupWrapperServer parameterized
// by the tmux-command path. Tests that want a non-default wrapper
// (e.g. one that fails has-session with a non-1 exit code) write
// their own script first and call this helper instead.
func setupWrapperServerWithScript(
	t *testing.T, script string,
) (client *apiclient.Client, baseURL string) {
	t.Helper()
	client, baseURL, _ = setupWrapperServerWithScriptAndDB(
		t, script,
	)
	return client, baseURL
}

func setupWrapperServerWithScriptAndDB(
	t *testing.T, script string,
) (client *apiclient.Client, baseURL string, database *db.DB) {
	t.Helper()
	client, baseURL, database, _ = setupWrapperServerWithScriptAndDBAndServer(
		t, script,
	)
	return client, baseURL, database
}

func setupWrapperServerWithScriptAndDBAndServer(
	t *testing.T, script string,
) (
	client *apiclient.Client,
	baseURL string,
	database *db.DB,
	srv *Server,
) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e tests skipped in short mode")
	}

	dir := t.TempDir()
	database = dbtest.Open(t)

	bareDir := filepath.Join(dir, "clones")
	require.NoError(t, os.MkdirAll(bareDir, 0o755))
	clones := gitclone.New(bareDir, nil)
	bare, err := clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(t.Context(), "repo-acme-widget"),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(t, err)
	tmpWork := filepath.Join(dir, "work")
	gitfixture.Run(t, dir, "init", "--bare", "--initial-branch=main", bare)
	gitfixture.Run(t, dir, "clone", bare, tmpWork)
	gitfixture.Run(t, tmpWork, "config", "user.email", "test@test.com")
	gitfixture.Run(t, tmpWork, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpWork, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	gitfixture.Run(t, tmpWork, "add", ".")
	gitfixture.Run(t, tmpWork, "commit", "-m", "base commit")
	gitfixture.Run(t, tmpWork, "push", "origin", "main")
	gitfixture.Run(t, tmpWork, "checkout", "-b", "feature")
	require.NoError(t, os.WriteFile(
		filepath.Join(tmpWork, "new.txt"),
		[]byte("new\n"), 0o644,
	))
	gitfixture.Run(t, tmpWork, "add", ".")
	gitfixture.Run(t, tmpWork, "commit", "-m", "feature commit")
	gitfixture.Run(t, tmpWork, "push", "origin", "feature")

	// Point bare origin at itself so EnsureClone fetch works.
	gitfixture.Run(t, bare, "remote", "add", "origin", bare)

	worktreeDir := filepath.Join(dir, "worktrees")

	repos := []ghclient.RepoRef{
		{Platform: "github", Owner: "acme", Name: "widget", PlatformHost: "github.com"},
	}
	mock := &mockGH{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database, nil, repos, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	cfg := &config.Config{
		Tmux: config.Tmux{
			Command: []string{script, "wrap"},
		},
	}
	srv = New(database, syncer, nil, "/", cfg, ServerOptions{
		Clones:      clones,
		WorktreeDir: worktreeDir,
	})
	seedPR(t, database, "acme", "widget", 1)

	// Real listener — WebSocket Dial needs a real TCP endpoint.
	// The generated API client also points at this URL rather than
	// the in-process roundtripper used elsewhere, because we cannot
	// split HTTP and WebSocket transports per-request.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	baseURL = "http://" + ln.Addr().String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		select {
		case err := <-serveErr:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-ctx.Done():
			require.FailNow(t, "workspace wrapper server did not stop")
		}
	})

	// Wrap the underlying TCP transport with the same Content-Type
	// shim setupTestClient uses — the server's CSRF check rejects
	// non-GET requests without Content-Type (e.g. DELETE with no
	// body) as 415 Unsupported Media Type. The shim runs in addition
	// to the normal transport, which still reaches the httptest
	// server over TCP so WebSocket upgrades continue to work.
	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet && req.Header.Get("Content-Type") == "" {
				req.Header.Set("Content-Type", "application/json")
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
	client, err = apiclient.NewWithHTTPClient(baseURL, httpClient)
	require.NoError(t, err)

	return client, baseURL, database, srv
}

func TestTmuxWrapperNewSession(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	client, _, record := setupWrapperServer(t)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		t.Context(),
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

	// Workspace setup runs asynchronously. Poll the record file
	// until the new-session invocation shows up, up to ~5s.
	var argvs [][]string
	require.Eventually(
		func() bool {
			argvs = readTmuxRecord(t, record)
			for _, argv := range argvs {
				if len(argv) >= 2 && argv[1] == "new-session" {
					return true
				}
			}
			return false
		},
		5*time.Second, 50*time.Millisecond,
		"new-session argv not recorded",
	)

	var newSession []string
	for _, argv := range argvs {
		if len(argv) >= 2 && argv[1] == "new-session" {
			newSession = argv
			break
		}
	}

	// "wrap" prefix, then "new-session -d -s <id> -c <path> <handoff>"
	// where <handoff> is the /bin/sh env-file bootstrap that delivers
	// the credential-sanitized environment and login shell to the pane.
	require.Len(newSession, 8)
	assert.Equal("wrap", newSession[0])
	assert.Equal("new-session", newSession[1])
	assert.Equal("-d", newSession[2])
	assert.Equal("-s", newSession[3])
	assert.NotEmpty(newSession[4])
	assert.Equal("-c", newSession[5])
	assert.NotEmpty(newSession[6])
	assert.Contains(newSession[7], "/bin/sh ")
}

func TestWorkspaceResponseIncludesTmuxWorkingState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	client, _, record := setupWrapperServer(t)
	setTmuxRecorderPaneTitle(t, record, "⠴ t3code-b5014b03")
	setTmuxRecorderPaneOutput(t, record, "stable output")
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	waitForWorkspaceReady(t, ctx, client, wsID)

	var got struct {
		TmuxPaneTitle      *string `json:"tmux_pane_title"`
		TmuxWorking        bool    `json:"tmux_working"`
		TmuxActivitySource string  `json:"tmux_activity_source"`
		TmuxLastOutputAt   *string `json:"tmux_last_output_at"`
	}
	require.Eventually(func() bool {
		got = getRawWorkspaceActivity(t, client, ctx, wsID)
		return got.TmuxPaneTitle != nil && got.TmuxWorking
	}, time.Second, 10*time.Millisecond)
	require.NotNil(got.TmuxPaneTitle)
	assert.Equal("⠴ t3code-b5014b03", *got.TmuxPaneTitle)
	assert.True(got.TmuxWorking)

	var listed struct {
		Workspaces []struct {
			ID            string  `json:"id"`
			TmuxPaneTitle *string `json:"tmux_pane_title"`
			TmuxWorking   bool    `json:"tmux_working"`
		} `json:"workspaces"`
	}
	require.Eventually(func() bool {
		listResp, listErr := client.HTTP.ListWorkspaces(ctx)
		require.NoError(listErr)
		defer listResp.Body.Close()
		require.Equal(http.StatusOK, listResp.StatusCode)
		listed.Workspaces = nil
		require.NoError(json.NewDecoder(listResp.Body).Decode(&listed))
		return len(listed.Workspaces) == 1 &&
			listed.Workspaces[0].TmuxPaneTitle != nil &&
			listed.Workspaces[0].TmuxWorking
	}, time.Second, 10*time.Millisecond)
	require.Len(listed.Workspaces, 1)
	assert.Equal(wsID, listed.Workspaces[0].ID)
	require.NotNil(listed.Workspaces[0].TmuxPaneTitle)
	assert.Equal("⠴ t3code-b5014b03", *listed.Workspaces[0].TmuxPaneTitle)
	assert.True(listed.Workspaces[0].TmuxWorking)
}

func TestFilteredActivityIncrementalPollRetainsWorkspaceSubject(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	script, record := writeTmuxRecorder(t)
	setTmuxRecorderPaneOutput(t, record, "baseline output")
	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(t, script)
	srv.cfgMu.Lock()
	srv.cfg.Activity.UseWorkspaceActivityForRecency = true
	srv.cfgMu.Unlock()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	repo, err := database.GetRepoByIdentity(ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"))
	require.NoError(err)
	require.NotNil(repo)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1)
	require.NoError(err)
	require.NotNil(mr)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mr.ID,
		EventType:      "issue_comment",
		Author:         "search-reviewer",
		Body:           "matches only provider event fields",
		CreatedAt:      now,
		DedupeKey:      "searched-workspace-comment",
	}}))

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	workspaceID := createResp.JSON202.Id
	waitForWorkspaceReady(t, ctx, client, workspaceID)

	require.Eventually(func() bool {
		activity := getRawWorkspaceActivity(t, client, ctx, workspaceID)
		return activity.TmuxActivitySource == "none" && activity.TmuxLastOutputAt == nil
	}, 3*time.Second, 50*time.Millisecond, "tmux baseline was not observed")
	setTmuxRecorderPaneOutput(t, record, "changed output")

	search := "search-reviewer"
	since := now.Add(-time.Hour).Format(time.RFC3339)
	probe, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Search: &search, Since: &since},
	)
	require.NoError(err)
	require.Equalf(http.StatusOK, probe.StatusCode(), "activity response: %s", probe.Body)
	var initial *generated.ListActivityResponse
	require.Eventually(func() bool {
		response, requestErr := client.HTTP.ListActivityWithResponse(
			ctx, &generated.ListActivityParams{Search: &search, Since: &since},
		)
		if requestErr != nil || response.JSON200 == nil || response.JSON200.WorkspaceActivity == nil {
			return false
		}
		initial = response
		return len(response.JSON200.WorkspaceActivity) == 1
	}, 8*time.Second, 100*time.Millisecond, "workspace activity did not observe changed tmux output")
	require.NotNil(initial)
	require.NotNil(initial.JSON200)
	require.NotNil(initial.JSON200.Items)
	require.Len(initial.JSON200.Items, 1)
	require.NotNil(initial.JSON200.WorkspaceActivity)
	require.Len(initial.JSON200.WorkspaceActivity, 1)
	initialWorkspace := initial.JSON200.WorkspaceActivity[0]
	assert.EqualValues(1, initialWorkspace.ItemNumber)
	require.NotNil(initialWorkspace.Workspace)
	assert.Equal(workspaceID, initialWorkspace.Workspace.Id)

	after := initial.JSON200.Items[0].Cursor
	incremental, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Search: &search, Since: &since, After: &after},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, incremental.StatusCode())
	require.NotNil(incremental.JSON200)
	require.NotNil(incremental.JSON200.Items)
	assert.Empty(incremental.JSON200.Items)
	require.NotNil(incremental.JSON200.WorkspaceActivity)
	require.Len(incremental.JSON200.WorkspaceActivity, 1)
	incrementalWorkspace := incremental.JSON200.WorkspaceActivity[0]
	assert.EqualValues(1, incrementalWorkspace.ItemNumber)
	require.NotNil(incrementalWorkspace.Workspace)
	assert.Equal(workspaceID, incrementalWorkspace.Workspace.Id)

	author := mr.Author
	authorInitial, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Author: &author, Since: &since},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, authorInitial.StatusCode())
	require.NotNil(authorInitial.JSON200)
	require.NotNil(authorInitial.JSON200.Items)
	require.NotEmpty(authorInitial.JSON200.Items)

	authorAfter := authorInitial.JSON200.Items[0].Cursor
	authorIncremental, err := client.HTTP.ListActivityWithResponse(
		ctx, &generated.ListActivityParams{Author: &author, Since: &since, After: &authorAfter},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, authorIncremental.StatusCode())
	require.NotNil(authorIncremental.JSON200)
	require.NotNil(authorIncremental.JSON200.Items)
	assert.Empty(authorIncremental.JSON200.Items)
	require.NotNil(authorIncremental.JSON200.WorkspaceActivity)
	require.Len(authorIncremental.JSON200.WorkspaceActivity, 1)
	authorWorkspace := authorIncremental.JSON200.WorkspaceActivity[0]
	assert.EqualValues(1, authorWorkspace.ItemNumber)
	require.NotNil(authorWorkspace.Workspace)
	assert.Equal(workspaceID, authorWorkspace.Workspace.Id)
}

func TestActivityAuthorsIncludeWorkspaceOnlySubject(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	script, record := writeTmuxRecorder(t)
	setTmuxRecorderPaneOutput(t, record, "baseline output")
	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(t, script)
	srv.cfgMu.Lock()
	srv.cfg.Activity.UseWorkspaceActivityForRecency = true
	srv.cfgMu.Unlock()
	ctx := t.Context()

	repo, err := database.GetRepoByIdentity(
		ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1)
	require.NoError(err)
	require.NotNil(mr)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)

	require.Eventually(func() bool {
		activity := getRawWorkspaceActivity(t, client, ctx, createResp.JSON202.Id)
		return activity.TmuxActivitySource == "none" && activity.TmuxLastOutputAt == nil
	}, 3*time.Second, 50*time.Millisecond, "tmux baseline was not observed")
	since := time.Now().UTC().Format(time.RFC3339Nano)
	setTmuxRecorderPaneOutput(t, record, "workspace-only activity")

	params := &generated.ListActivityAuthorsParams{Since: &since}
	var response *generated.ListActivityAuthorsResponse
	require.Eventually(func() bool {
		got, requestErr := client.HTTP.ListActivityAuthorsWithResponse(ctx, params)
		if requestErr != nil || got.JSON200 == nil || got.JSON200.Authors == nil {
			return false
		}
		response = got
		for _, author := range got.JSON200.Authors {
			if strings.EqualFold(author, mr.Author) {
				return true
			}
		}
		return false
	}, 8*time.Second, 100*time.Millisecond, "workspace-only author was not listed")
	require.NotNil(response)
	require.NotNil(response.JSON200)
	require.NotNil(response.JSON200.Authors)
	assert.Equal([]string{mr.Author}, response.JSON200.Authors)

	missingRepo := "github|github.com/acme/missing"
	params.Repo = &missingRepo
	scoped, err := client.HTTP.ListActivityAuthorsWithResponse(ctx, params)
	require.NoError(err)
	require.Equal(http.StatusOK, scoped.StatusCode())
	require.NotNil(scoped.JSON200)
	require.NotNil(scoped.JSON200.Authors)
	assert.Empty(scoped.JSON200.Authors)

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	params.Repo = nil
	params.Since = &future
	outOfRange, err := client.HTTP.ListActivityAuthorsWithResponse(ctx, params)
	require.NoError(err)
	require.Equal(http.StatusOK, outOfRange.StatusCode())
	require.NotNil(outOfRange.JSON200)
	require.NotNil(outOfRange.JSON200.Authors)
	assert.Empty(outOfRange.JSON200.Authors)
}

func TestFederatedActivityIncludesNodeWorkspaceOnlySubject(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	script, record := writeTmuxRecorder(t)
	setTmuxRecorderPaneOutput(t, record, "baseline output")
	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(t, script)
	srv.cfgMu.Lock()
	srv.cfg.Fleet.Enabled = true
	srv.cfgMu.Unlock()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		t.Context(),
		generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	waitForWorkspaceReady(t, t.Context(), client, createResp.JSON202.Id)
	require.Eventually(func() bool {
		activity := getRawWorkspaceActivity(t, client, t.Context(), createResp.JSON202.Id)
		return activity.TmuxActivitySource == "none" && activity.TmuxLastOutputAt == nil
	}, 3*time.Second, 50*time.Millisecond)

	providerClient := providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		require.Equal(federationauth.ScopeProviderRead, scope)
		body := `{"items":[],"item_activity":[],"workspace_activity":[],"use_workspace_activity_for_recency":true}`
		if request.URL.Path == "/api/v1/activity/authors" {
			body = `{"authors":["hub author"],"use_workspace_activity_for_recency":true}`
		} else {
			require.Equal("/api/v1/activity", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	srv.providerSource = &hubProviderSource{client: providerClient}
	srv.providerProxy = newProviderProxy(providerClient)
	srv.providerRouteSpoke = true

	since := time.Now().UTC().Format(time.RFC3339Nano)
	setTmuxRecorderPaneOutput(t, record, "workspace-only activity")
	var response *generated.ListActivityResponse
	require.Eventually(func() bool {
		got, requestErr := client.HTTP.ListActivityWithResponse(
			t.Context(), &generated.ListActivityParams{Since: &since},
		)
		if requestErr != nil || got.JSON200 == nil || got.JSON200.WorkspaceActivity == nil {
			return false
		}
		response = got
		return len(got.JSON200.WorkspaceActivity) == 1
	}, 8*time.Second, 100*time.Millisecond)
	require.NotNil(response)
	require.Len(response.JSON200.WorkspaceActivity, 1)
	require.Equal(createResp.JSON202.Id, response.JSON200.WorkspaceActivity[0].Workspace.Id)

	repo, err := database.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repo.ID, 1)
	require.NoError(err)
	require.NotNil(mr)
	authors, err := client.HTTP.ListActivityAuthorsWithResponse(
		t.Context(), &generated.ListActivityAuthorsParams{Since: &since},
	)
	require.NoError(err)
	require.Equal(http.StatusOK, authors.StatusCode())
	require.NotNil(authors.JSON200)
	require.NotNil(authors.JSON200.Authors)
	require.ElementsMatch([]string{"hub author", mr.Author}, authors.JSON200.Authors)
}

func TestWorkspaceActivityNumberSearchIncludesEventlessSubject(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	script, record := writeTmuxRecorder(t)
	setTmuxRecorderPaneOutput(t, record, "baseline output")
	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(t, script)
	srv.cfgMu.Lock()
	srv.cfg.Activity.UseWorkspaceActivityForRecency = true
	srv.cfgMu.Unlock()
	ctx := t.Context()

	repo, err := database.GetRepoByIdentity(
		ctx, verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1)
	require.NoError(err)
	require.NotNil(mr)
	mr.Title = "Unrelated title"
	_, err = database.UpsertMergeRequest(ctx, mr)
	require.NoError(err)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		ctx,
		generated.CreateWorkspaceInputBody{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", MrNumber: 1,
		},
	)
	require.NoError(err)
	require.Equal(http.StatusAccepted, createResp.StatusCode())
	require.NotNil(createResp.JSON202)
	workspaceID := createResp.JSON202.Id
	waitForWorkspaceReady(t, ctx, client, workspaceID)

	require.Eventually(func() bool {
		activity := getRawWorkspaceActivity(t, client, ctx, workspaceID)
		return activity.TmuxActivitySource == "none" && activity.TmuxLastOutputAt == nil
	}, 3*time.Second, 50*time.Millisecond, "tmux baseline was not observed")
	since := time.Now().UTC().Format(time.RFC3339Nano)
	setTmuxRecorderPaneOutput(t, record, "changed output")

	for _, search := range []string{"1", "#1"} {
		var response *generated.ListActivityResponse
		require.Eventually(func() bool {
			got, requestErr := client.HTTP.ListActivityWithResponse(
				ctx, &generated.ListActivityParams{Search: &search, Since: &since},
			)
			if requestErr != nil || got.JSON200 == nil || got.JSON200.WorkspaceActivity == nil {
				return false
			}
			response = got
			return len(got.JSON200.WorkspaceActivity) == 1
		}, 8*time.Second, 100*time.Millisecond, "workspace activity did not match %q", search)
		require.NotNil(response)
		require.NotNil(response.JSON200)
		require.NotNil(response.JSON200.Items)
		assert.Empty(response.JSON200.Items, "provider Activity must remain outside the window")
		require.NotNil(response.JSON200.WorkspaceActivity)
		require.Len(response.JSON200.WorkspaceActivity, 1)
		workspaceSubject := response.JSON200.WorkspaceActivity[0]
		assert.EqualValues(1, workspaceSubject.ItemNumber)
		assert.Equal("Unrelated title", workspaceSubject.ItemTitle)
		require.NotNil(workspaceSubject.Workspace)
		assert.Equal(workspaceID, workspaceSubject.Workspace.Id)
	}
}

func getRawWorkspaceActivity(
	t *testing.T,
	client *apiclient.Client,
	ctx context.Context,
	wsID string,
) struct {
	TmuxPaneTitle      *string `json:"tmux_pane_title"`
	TmuxWorking        bool    `json:"tmux_working"`
	TmuxActivitySource string  `json:"tmux_activity_source"`
	TmuxLastOutputAt   *string `json:"tmux_last_output_at"`
} {
	t.Helper()
	resp, err := client.HTTP.GetWorkspace(ctx, wsID)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		TmuxPaneTitle      *string `json:"tmux_pane_title"`
		TmuxWorking        bool    `json:"tmux_working"`
		TmuxActivitySource string  `json:"tmux_activity_source"`
		TmuxLastOutputAt   *string `json:"tmux_last_output_at"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return got
}

func TestIsWorkingTmuxTitleDetectsCodexSpinner(t *testing.T) {
	assert := assert.New(t)

	cases := []struct {
		name    string
		title   string
		working bool
	}{
		{
			name:    "codex spinner frame",
			title:   "⠴ t3code-b5014b03",
			working: true,
		},
		{
			name:    "another codex spinner frame",
			title:   "⠦ t3code-b5014b03",
			working: true,
		},
		{
			name:    "settled codex title",
			title:   "t3code-b5014b03",
			working: false,
		},
		{
			name:    "english busy title is not protocol",
			title:   "codex working",
			working: false,
		},
		{
			name:    "opencode style title is not protocol",
			title:   "OC | Run sleep 10",
			working: false,
		},
		{
			name:    "pi style title is not protocol",
			title:   "π - tmp.foo",
			working: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(tc.working, workspaceapi.IsWorkingTmuxTitle(tc.title))
		})
	}
}

func TestWorkspaceCreateFailureLogsAndPersistsAuditEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    echo "wrapper failed" >&2` + "\n" +
		`    exit 42` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	var logBuf lockedBuffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	client, _, database := setupWrapperServerWithScriptAndDB(
		t, script,
	)
	ctx := t.Context()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	failed := waitForWorkspaceStatus(t, ctx, client, wsID, "error")
	require.NotNil(failed.ErrorMessage)
	assert.Contains(*failed.ErrorMessage, "tmux new-session")
	assert.Contains(*failed.ErrorMessage, "wrapper failed")

	rows, err := database.ReadDB().QueryContext(ctx, `
		SELECT stage, outcome, message
		FROM forge_workspace_setup_events
		WHERE workspace_id = ?
		ORDER BY id`, wsID,
	)
	require.NoError(err)
	defer rows.Close()

	type auditEvent struct {
		stage   string
		outcome string
		message string
	}

	var events []auditEvent
	for rows.Next() {
		var ev auditEvent
		require.NoError(rows.Scan(&ev.stage, &ev.outcome, &ev.message))
		events = append(events, ev)
	}
	require.NoError(rows.Err())
	require.NotEmpty(events)
	last := events[len(events)-1]
	assert.Equal("tmux_session", last.stage)
	assert.Equal("failure", last.outcome)
	assert.Contains(last.message, "wrapper failed")

	logs := logBuf.String()
	assert.Contains(logs, "workspace setup failed")
	assert.Contains(logs, wsID)
	assert.Contains(logs, "tmux_session")
}

func TestWorkspaceShutdownCancellationPersistsFailureViaAPI(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	assert := assert.New(t)
	require := require.New(t)

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
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    while :; do sleep 1; done` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(
		t, script,
	)
	ctx := t.Context()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			argvs := readTmuxRecord(t, record)
			for _, argv := range argvs {
				if len(argv) >= 2 && argv[1] == "new-session" {
					return true
				}
			}
			return false
		},
		5*time.Second,
		50*time.Millisecond,
	)

	shutdownCtx, cancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer cancel()
	require.NoError(srv.Shutdown(shutdownCtx))

	restartSyncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockGH{}},
		database, nil, defaultTestRepos, time.Minute, nil, nil,
	)
	t.Cleanup(restartSyncer.Stop)
	restarted := New(
		database, restartSyncer, nil, "/",
		nil, ServerOptions{WorktreeDir: filepath.Join(dir, "restart-worktrees")},
	)
	t.Cleanup(func() { gracefulShutdown(t, restarted) })
	restartedClient := setupTestClient(t, restarted)

	getResp, err := restartedClient.HTTP.GetWorkspaceWithResponse(
		ctx, wsID,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, getResp.StatusCode())
	require.NotNil(getResp.JSON200)
	assert.Equal("error", getResp.JSON200.Status)
	require.NotNil(getResp.JSON200.ErrorMessage)
	assert.Contains(*getResp.JSON200.ErrorMessage, "tmux new-session")

	rows, err := database.ReadDB().QueryContext(ctx, `
		SELECT stage, outcome, message
		FROM forge_workspace_setup_events
		WHERE workspace_id = ?
		ORDER BY id`, wsID,
	)
	require.NoError(err)
	defer rows.Close()

	type auditEvent struct {
		stage   string
		outcome string
		message string
	}

	var events []auditEvent
	for rows.Next() {
		var ev auditEvent
		require.NoError(rows.Scan(&ev.stage, &ev.outcome, &ev.message))
		events = append(events, ev)
	}
	require.NoError(rows.Err())
	require.Len(events, 2)
	assert.Equal("setup", events[0].stage)
	assert.Equal("started", events[0].outcome)
	assert.Equal("tmux_session", events[1].stage)
	assert.Equal("failure", events[1].outcome)
	assert.Contains(events[1].message, "tmux new-session")
}

func TestWorkspaceSetupFailureRollbackCleansWorktreeViaAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    echo "wrapper failed" >&2` + "\n" +
		`    exit 42` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, database, srv := setupWrapperServerWithScriptAndDBAndServer(
		t, script,
	)
	ctx := t.Context()
	clonePath, err := srv.clones.ClonePathForContext(
		gitclone.WithRepositoryIdentity(ctx, "repo-acme-widget"),
		"github", "github.com", "acme", "widget",
	)
	require.NoError(err)
	featureSHA := gitfixture.SHA(t, clonePath, "refs/heads/feature")

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	var failed *generated.WorkspaceResponse
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			require.NoError(getErr)
			if getResp.StatusCode() != http.StatusOK || getResp.JSON200 == nil {
				return false
			}
			if getResp.JSON200.Status != "error" {
				return false
			}
			failed = getResp.JSON200
			return true
		},
		5*time.Second,
		50*time.Millisecond,
	)

	require.NotNil(failed)
	assert.Equal(featureSHA, gitfixture.SHA(t, clonePath, "refs/heads/feature"))
	assert.Eventually(
		func() bool {
			_, err := os.Stat(failed.WorktreePath)
			return os.IsNotExist(err)
		},
		5*time.Second,
		50*time.Millisecond,
	)

	stored, err := database.GetWorkspace(ctx, wsID)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("error", stored.Status)
	assert.Empty(stored.WorkspaceBranch)
}

func TestWorkspaceRetryWhileCreatingQueuesAndRunsAfterFailureViaAPI(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	release := filepath.Join(dir, "release-first")
	countFile := filepath.Join(dir, "new-session-count")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		"TMUX_RELEASE=" + shellquote.Join(release) + "\n" +
		"TMUX_COUNT=" + shellquote.Join(countFile) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    count=0` + "\n" +
		`    if [ -f "$TMUX_COUNT" ]; then count=$(cat "$TMUX_COUNT"); fi` + "\n" +
		`    count=$((count + 1))` + "\n" +
		`    printf '%s' "$count" > "$TMUX_COUNT"` + "\n" +
		`    if [ "$count" = "1" ]; then` + "\n" +
		`      while [ ! -f "$TMUX_RELEASE" ]; do sleep 0.05; done` + "\n" +
		`      echo "first setup failed" >&2` + "\n" +
		`      exit 42` + "\n" +
		`    fi` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, database := setupWrapperServerWithScriptAndDB(
		t, script,
	)
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			argvs := readTmuxRecord(t, record)
			for _, argv := range argvs {
				if len(argv) >= 2 && argv[1] == "new-session" {
					return true
				}
			}
			return false
		},
		5*time.Second,
		50*time.Millisecond,
	)

	retryResp, err := client.HTTP.RetryWorkspaceWithResponse(ctx, wsID)
	require.NoError(err)
	require.Equal(http.StatusAccepted, retryResp.StatusCode())
	require.NotNil(retryResp.JSON202)
	assert.Equal("creating", retryResp.JSON202.Status)

	require.NoError(os.WriteFile(release, []byte("go\n"), 0o644))

	var ready *generated.WorkspaceResponse
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			require.NoError(getErr)
			if getResp.StatusCode() != http.StatusOK || getResp.JSON200 == nil {
				return false
			}
			if getResp.JSON200.Status != "ready" {
				return false
			}
			ready = getResp.JSON200
			return true
		},
		15*time.Second,
		50*time.Millisecond,
	)
	require.NotNil(ready)
	assert.Nil(ready.ErrorMessage)

	argvs := readTmuxRecord(t, record)
	var newSessionCount int
	for _, argv := range argvs {
		if len(argv) >= 2 && argv[1] == "new-session" {
			newSessionCount++
		}
	}
	assert.Equal(2, newSessionCount)

	events, err := database.ListWorkspaceSetupEvents(ctx, wsID)
	require.NoError(err)
	var retryEvents int
	for _, event := range events {
		if event.Stage == "setup" && event.Outcome == "retrying" {
			retryEvents++
		}
	}
	assert.Equal(1, retryEvents)
}

func TestWorkspaceShutdownCancellationDoesNotPersistAfterDeadlineBudgetExhausted(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	assert := assert.New(t)
	require := require.New(t)

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
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    while :; do sleep 1; done` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, baseURL, database, srv := setupWrapperServerWithScriptAndDBAndServer(
		t, script,
	)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		t.Context(),
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			argvs := readTmuxRecord(t, record)
			for _, argv := range argvs {
				if len(argv) >= 2 && argv[1] == "new-session" {
					return true
				}
			}
			return false
		},
		5*time.Second,
		50*time.Millisecond,
	)

	tx, err := database.WriteDB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })

	origHandler := srv.handler
	blockStarted := make(chan struct{}, 1)
	blockRelease := make(chan struct{})
	srv.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/block" {
			select {
			case blockStarted <- struct{}{}:
			default:
			}
			<-blockRelease
			w.WriteHeader(http.StatusOK)
			return
		}
		origHandler.ServeHTTP(w, r)
	})

	blockErrCh := make(chan error, 1)
	go func() {
		resp, err := http.Get(baseURL + "/block")
		if err == nil && resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		blockErrCh <- err
	}()

	select {
	case <-blockStarted:
	case <-time.After(2 * time.Second):
		require.FailNow("blocking request never started")
	}

	time.AfterFunc(250*time.Millisecond, func() {
		close(blockRelease)
	})

	shutdownCtx, cancel := context.WithTimeout(
		t.Context(), 400*time.Millisecond,
	)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	require.ErrorIs(err, context.DeadlineExceeded)

	require.NoError(tx.Rollback())

	require.NoError(<-blockErrCh)

	ws, err := database.GetWorkspace(t.Context(), wsID)
	require.NoError(err)
	require.NotNil(ws)
	assert.Equal("creating", ws.Status)
	assert.Nil(ws.ErrorMessage)

	events, err := database.ListWorkspaceSetupEvents(
		t.Context(), wsID,
	)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal("setup", events[0].Stage)
	assert.Equal("started", events[0].Outcome)

	longCtx, longCancel := context.WithTimeout(
		t.Context(), 2*time.Second,
	)
	defer longCancel()
	require.NoError(srv.Shutdown(longCtx))
}

func TestTmuxWrapperAttachSession(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	client, baseURL, record := setupWrapperServer(t)
	ctx := t.Context()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	// Poll for status == "ready".
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "ready"
		},
		5*time.Second, 50*time.Millisecond,
	)

	// Connect to the WebSocket terminal endpoint using the
	// httptest baseURL (the generated client cannot upgrade to
	// WebSocket, so we dial directly with coder/websocket).
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) +
		"/ws/v1/workspaces/" + wsID + "/terminal"
	dialCtx, dialCancel := context.WithTimeout(
		ctx, 3*time.Second,
	)
	defer dialCancel()
	u, err := url.Parse(wsURL)
	require.NoError(err)
	conn, httpResp, err := websocket.Dial(
		dialCtx, u.String(), nil,
	)
	require.NoError(err)
	if httpResp != nil && httpResp.Body != nil {
		httpResp.Body.Close()
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// The recording script exits 0 immediately, so the PTY
	// closes and the handler sends an "exited" message. Read
	// until the connection closes or 3s elapses.
	readCtx, readCancel := context.WithTimeout(
		ctx, 3*time.Second,
	)
	defer readCancel()
	for {
		_, _, readErr := conn.Read(readCtx)
		if readErr != nil {
			break
		}
	}

	// The recorded argv should contain an attach-session invocation
	// with our "wrap" prefix.
	var attach []string
	for _, argv := range readTmuxRecord(t, record) {
		if len(argv) >= 3 &&
			argv[1] == "-u" &&
			argv[2] == "attach-session" {
			attach = argv
			break
		}
	}
	require.NotNil(attach, "attach-session argv not recorded")
	require.Len(attach, 6)
	assert.Equal("wrap", attach[0])
	assert.Equal("-u", attach[1])
	assert.Equal("attach-session", attach[2])
	assert.Equal("-E", attach[3])
	assert.Equal("-t", attach[4])
	assert.NotEmpty(attach[5])
}

func TestTerminalRouteE2EPropagatesWorkspaceID(t *testing.T) {
	assert := assert.New(t)
	_, baseURL, _ := setupWrapperServer(t)

	resp, err := http.Get(
		baseURL + "/api/v1/workspaces/not-present/terminal",
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(http.StatusNotFound, resp.StatusCode)
	assert.Contains(string(body), "workspace not found")
}

func TestWorkspaceSetupResourceExhaustionGetsHelpfulErrorViaAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    echo "fork/exec /opt/homebrew/bin/tmux: resource temporarily unavailable" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, _ := setupWrapperServerWithScriptAndDB(t, script)
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	var failed *generated.WorkspaceResponse
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			require.NoError(getErr)
			if getResp.StatusCode() != http.StatusOK || getResp.JSON200 == nil {
				return false
			}
			if getResp.JSON200.Status != "error" {
				return false
			}
			failed = getResp.JSON200
			return true
		},
		5*time.Second, 50*time.Millisecond,
	)
	require.NotNil(failed)
	require.NotNil(failed.ErrorMessage)
	assert.Contains(*failed.ErrorMessage, "host process limit reached")
}

func TestWorkspaceListReturnsWhileSubprocessCapacityIsHeld(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	restoreLimiter := procutil.SetDefaultLimiterForTest(
		procutil.NewLimiterWithAcquireTimeout(1, 500*time.Millisecond),
	)
	t.Cleanup(restoreLimiter)

	client, _, _ := setupWrapperServer(t)
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	waitForWorkspaceReady(t, ctx, client, createResp.JSON202.Id)

	releaseHeld, err := procutil.TryAcquire(
		context.Background(), "test-held subprocess capacity",
	)
	require.NoError(err)
	defer releaseHeld()

	type listResult struct {
		resp *http.Response
		err  error
	}
	listDone := make(chan listResult, 1)
	go func() {
		resp, listErr := client.HTTP.ListWorkspaces(ctx)
		listDone <- listResult{resp: resp, err: listErr}
	}()

	select {
	case got := <-listDone:
		require.NoError(got.err)
		defer got.resp.Body.Close()
		require.Equal(http.StatusOK, got.resp.StatusCode)

		var listed struct {
			Workspaces []struct {
				ID string `json:"id"`
			} `json:"workspaces"`
		}
		require.NoError(json.NewDecoder(got.resp.Body).Decode(&listed))
		require.Len(listed.Workspaces, 1)
		assert.Equal(createResp.JSON202.Id, listed.Workspaces[0].ID)
	case <-time.After(200 * time.Millisecond):
		require.Fail("workspace list waited for subprocess capacity")
	}

	releaseHeld()
}

func TestWorkspaceSetupLimiterTimeoutSurfacesResourceExhaustionViaAPI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	restoreLimiter := procutil.SetDefaultLimiterForTest(
		procutil.NewLimiterWithAcquireTimeout(1, 25*time.Millisecond),
	)
	t.Cleanup(restoreLimiter)

	client, _, _ := setupWrapperServer(t)
	ctx := context.Background()

	releaseHeld, err := procutil.TryAcquire(
		context.Background(), "test-held subprocess capacity",
	)
	require.NoError(err)
	defer releaseHeld()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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

	var failed *generated.WorkspaceResponse
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, createResp.JSON202.Id,
			)
			require.NoError(getErr)
			if getResp.StatusCode() != http.StatusOK || getResp.JSON200 == nil {
				return false
			}
			if getResp.JSON200.Status != "error" {
				return false
			}
			failed = getResp.JSON200
			return true
		},
		5*time.Second, 25*time.Millisecond,
	)
	require.NotNil(failed)
	require.NotNil(failed.ErrorMessage)
	assert.Contains(*failed.ErrorMessage, "host process limit reached")
	assert.Contains(*failed.ErrorMessage, "subprocess capacity")
}

// TestReadTmuxRecordPreservesEmptyArgs pins down the parser's
// empty-arg handling. The NUL-delimited record format was chosen to
// round-trip argv with empty-string elements unambiguously; the
// parser must keep interior and trailing empties rather than
// collapsing them.
func TestReadTmuxRecordPreservesEmptyArgs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "record")

	// First record: 3 args with an interior empty ("a", "", "b").
	// Second record: 2 args with a trailing empty ("x", "").
	body := "3\x00a\x00\x00b\x00" + "2\x00x\x00\x00"
	require.NoError(os.WriteFile(path, []byte(body), 0o644))

	argvs := readTmuxRecord(t, path)
	require.Len(argvs, 2)
	assert.Equal([]string{"a", "", "b"}, argvs[0])
	assert.Equal([]string{"x", ""}, argvs[1])
}

// TestTmuxWrapperKillSession proves the configured tmux.command
// prefix reaches the kill-session exec issued by DELETE /workspaces/{id}.
// This complements TestTmuxWrapperNewSession and TestTmuxWrapperAttachSession —
// together they cover all three tmux verbs that cross the HTTP boundary.
func TestTmuxWrapperKillSession(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)
	client, _, record := setupWrapperServer(t)
	ctx := t.Context()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	// Poll for status == "ready" before deleting so the tmux
	// session is known to exist from the manager's perspective.
	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "ready"
		},
		5*time.Second, 50*time.Millisecond,
	)

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, delResp.StatusCode())

	// The recorded argv should contain a kill-session invocation
	// with our "wrap" prefix.
	var kill []string
	for _, argv := range readTmuxRecord(t, record) {
		if len(argv) >= 2 && argv[1] == "kill-session" {
			kill = argv
			break
		}
	}
	require.NotNil(kill, "kill-session argv not recorded")
	require.Len(kill, 4)
	assert.Equal("wrap", kill[0])
	assert.Equal("kill-session", kill[1])
	assert.Equal("-t", kill[2])
	assert.NotEmpty(kill[3])
}

func TestDeleteWorkspacePreservesRowWhenTmuxKillFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "permission denied" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, _ := setupWrapperServerWithScriptAndDB(t, script)
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "ready"
		},
		5*time.Second, 50*time.Millisecond,
	)

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusInternalServerError, delResp.StatusCode())
	require.NotNil(delResp.ApplicationproblemJSONDefault)
	require.NotNil(delResp.ApplicationproblemJSONDefault.Detail)
	assert.Contains(
		*delResp.ApplicationproblemJSONDefault.Detail,
		"kill tmux session",
	)
	assert.Contains(
		*delResp.ApplicationproblemJSONDefault.Detail,
		"permission denied",
	)

	getResp, err := client.HTTP.GetWorkspaceWithResponse(ctx, wsID)
	require.NoError(err)
	require.Equal(http.StatusOK, getResp.StatusCode())
	require.NotNil(getResp.JSON200)
	assert.Equal(wsID, getResp.JSON200.Id)
}

func TestDeleteWorkspaceTreatsTmuxServerExitAsGoneE2E(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "server exited unexpectedly" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, database := setupWrapperServerWithScriptAndDB(t, script)

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
		t.Context(),
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				t.Context(), wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "ready"
		},
		5*time.Second, 50*time.Millisecond,
	)

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		t.Context(), wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, delResp.StatusCode())

	getResp, err := client.HTTP.GetWorkspaceWithResponse(t.Context(), wsID)
	require.NoError(err)
	require.Equal(http.StatusNotFound, getResp.StatusCode())

	stored, err := database.GetWorkspace(t.Context(), wsID)
	require.NoError(err)
	assert.Nil(stored)
}

func TestDeleteErroredWorkspaceAllowsUnavailableTmux(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	body := "#!/bin/sh\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "new-session" ]; then` + "\n" +
		`    echo "tmux unavailable" >&2` + "\n" +
		`    exit 127` + "\n" +
		`  fi` + "\n" +
		`  if [ "$a" = "kill-session" ]; then` + "\n" +
		`    echo "tmux unavailable" >&2` + "\n" +
		`    exit 127` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(os.WriteFile(script, []byte(body), 0o755))

	client, _, _ := setupWrapperServerWithScriptAndDB(t, script)
	ctx := context.Background()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "error"
		},
		5*time.Second, 50*time.Millisecond,
	)

	force := true
	delResp, err := client.HTTP.DeleteWorkspaceWithResponse(
		ctx, wsID, &generated.DeleteWorkspaceParams{Force: &force},
	)
	require.NoError(err)
	require.Equal(http.StatusNoContent, delResp.StatusCode())

	getResp, err := client.HTTP.GetWorkspaceWithResponse(ctx, wsID)
	require.NoError(err)
	assert.Equal(http.StatusNotFound, getResp.StatusCode())
}

// TestTmuxWrapperAttachSurfacesWrapperFailure exercises the
// error-propagation path end-to-end. Workspace setup uses a wrapper
// that succeeds for new-session (so the workspace reaches "ready")
// but fails has-session with exit code 127 — the kind of exit a
// broken wrapper like systemd-run would produce. Under the old
// boolean-only tmuxSessionExists, this silently passed through as
// "session absent" and the bug hid behind a confusing new-session
// failure. With the bool/error split plus the exit-code-1 carve-out,
// the terminal handler sees the error and closes the WebSocket with
// StatusInternalError.
func TestTmuxWrapperAttachSurfacesWrapperFailure(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	record := filepath.Join(t.TempDir(), "record")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then exit 127; fi` + "\n" +
		"done\n" +
		"exit 0\n"
	attachWebsocketAndExpectInternalError(t, body)
}

// attachWebsocketAndExpectInternalError drives the end-to-end
// attach path with a custom fake-tmux script, asserting the
// WebSocket is closed by the handler with StatusInternalError
// rather than attaching to a session. Callers provide the script
// body; the helper handles server setup, workspace creation,
// ready-polling, dial, and close-status assertion.
func attachWebsocketAndExpectInternalError(t *testing.T, scriptBody string) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tmux")
	require.NoError(os.WriteFile(script, []byte(scriptBody), 0o755))

	client, baseURL := setupWrapperServerWithScript(t, script)
	ctx := t.Context()

	createResp, err := client.HTTP.CreateWorkspaceWithResponse(
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
	wsID := createResp.JSON202.Id

	require.Eventually(
		func() bool {
			getResp, getErr := client.HTTP.GetWorkspaceWithResponse(
				ctx, wsID,
			)
			if getErr != nil || getResp.JSON200 == nil {
				return false
			}
			return getResp.JSON200.Status == "ready"
		},
		5*time.Second, 50*time.Millisecond,
	)

	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) +
		"/ws/v1/workspaces/" + wsID + "/terminal"
	dialCtx, dialCancel := context.WithTimeout(
		ctx, 3*time.Second,
	)
	defer dialCancel()
	u, err := url.Parse(wsURL)
	require.NoError(err)
	conn, httpResp, err := websocket.Dial(
		dialCtx, u.String(), nil,
	)
	require.NoError(err)
	if httpResp != nil && httpResp.Body != nil {
		httpResp.Body.Close()
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	readCtx, readCancel := context.WithTimeout(
		ctx, 3*time.Second,
	)
	defer readCancel()
	_, _, readErr := conn.Read(readCtx)
	require.Error(readErr)
	assert.Equal(
		websocket.StatusInternalError,
		websocket.CloseStatus(readErr),
	)
}

// TestTmuxWrapperAttachSurfacesExit1Failure covers the second half
// of the session-absent heuristic at the HTTP layer: exit code 1
// without tmux's "can't find session" or "no server running"
// stderr must be treated as a real wrapper failure, not as
// "session absent, please create one." This is the common case the
// reviewer flagged — shell wrappers often exit 1 for their own
// generic errors.
func TestTmuxWrapperAttachSurfacesExit1Failure(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	record := filepath.Join(t.TempDir(), "record")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "wrapper failed" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	attachWebsocketAndExpectInternalError(t, body)
}

// TestTmuxWrapperAttachIgnoresAbsencePhraseOnStdout verifies that
// the absent-session heuristic is stderr-only at the HTTP layer:
// a wrapper that exits 1 with the tmux phrase on stdout (and an
// unrelated stderr message) must surface as an error, not as
// "session absent." Pairs with the unit-level
// TestManagerEnsureTmuxIgnoresAbsencePhraseOnStdout.
func TestTmuxWrapperAttachIgnoresAbsencePhraseOnStdout(t *testing.T) {
	runParallelServerTest(t)
	acquireRootWorkspaceGitSlot(t)

	record := filepath.Join(t.TempDir(), "record")
	body := "#!/bin/sh\n" +
		"TMUX_RECORD=" + shellquote.Join(record) + "\n" +
		`printf '%s\0' "$#" "$@" >> "$TMUX_RECORD"` + "\n" +
		`for a in "$@"; do` + "\n" +
		`  if [ "$a" = "has-session" ]; then` + "\n" +
		`    echo "can't find session: sim"` + "\n" + // stdout only
		`    echo "real failure" >&2` + "\n" +
		`    exit 1` + "\n" +
		`  fi` + "\n" +
		"done\n" +
		"exit 0\n"
	attachWebsocketAndExpectInternalError(t, body)
}
