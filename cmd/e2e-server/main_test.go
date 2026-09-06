package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/testsignal"
	"go.kenn.io/forge/internal/web"
)

var testTmux = newTestTmuxTracker()

type testTmuxTracker struct {
	mu           sync.Mutex
	commands     map[string][]string
	shuttingDown bool
}

func newTestTmuxTracker() *testTmuxTracker {
	return &testTmuxTracker{commands: make(map[string][]string)}
}

func TestMain(m *testing.M) {
	runCleanup, stopSignalCleanup := testsignal.Install(testTmux.cleanup, func(err error) {
		fmt.Fprintf(os.Stderr, "cleanup e2e-server test tmux: %v\n", err)
	})
	code := m.Run()
	if err := runCleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup e2e-server test tmux: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	stopSignalCleanup()
	os.Exit(code)
}

func (tracker *testTmuxTracker) start(
	tmuxCommand, args []string,
	afterStart func(),
) (func() error, error) {
	trackedCommand := testTmuxCommandIdentity(tmuxCommand)
	key := strings.Join(trackedCommand, "\x00")
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.shuttingDown {
		return nil, errors.New("e2e-server test tmux cleanup has started")
	}
	tracker.commands[key] = append([]string{}, trackedCommand...)
	output, err := procutil.Command(tmuxCommand[0], args...).CombinedOutput()
	if err != nil {
		delete(tracker.commands, key)
		return nil, fmt.Errorf("start e2e-server test tmux: %w: %s", err, output)
	}
	if afterStart != nil {
		afterStart()
	}
	return func() error { return tracker.stop(key, trackedCommand) }, nil
}

func testTmuxCommandIdentity(tmuxCommand []string) []string {
	if len(tmuxCommand) >= 3 && tmuxCommand[0] == filepath.Join(tmuxCommand[1], "tmux") {
		return tmuxCommand[2:]
	}
	return tmuxCommand
}

func (tracker *testTmuxTracker) cleanup() error {
	tracker.mu.Lock()
	tracker.shuttingDown = true
	commands := make(map[string][]string, len(tracker.commands))
	for key, command := range tracker.commands {
		commands[key] = append([]string{}, command...)
	}
	tracker.mu.Unlock()
	var cleanupErr error
	for key, command := range commands {
		cleanupErr = errors.Join(cleanupErr, tracker.stop(key, command))
	}
	return cleanupErr
}

func (tracker *testTmuxTracker) stop(key string, tmuxCommand []string) error {
	args := append([]string{}, tmuxCommand[1:]...)
	args = append(args, "kill-server")
	killOutput, killErr := procutil.Command(tmuxCommand[0], args...).CombinedOutput()
	args[len(args)-1] = "list-sessions"
	listOutput, listErr := procutil.Command(tmuxCommand[0], args...).CombinedOutput()
	if listErr == nil {
		return fmt.Errorf(
			"stop e2e-server test tmux: %v: %s; server still accepts commands: %s",
			killErr, killOutput, listOutput,
		)
	}
	if _, ok := errors.AsType[*exec.ExitError](listErr); !ok {
		return fmt.Errorf("verify e2e-server test tmux stopped: %w: %s", listErr, listOutput)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(listOutput)), "no server running on ") {
		return fmt.Errorf("verify e2e-server test tmux stopped: %w: %s", listErr, listOutput)
	}
	tracker.mu.Lock()
	if current, ok := tracker.commands[key]; ok && strings.Join(current, "\x00") == key {
		delete(tracker.commands, key)
	}
	tracker.mu.Unlock()
	return nil
}
func TestE2EWorkflowClientExercisesProviderWorkflowContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base, ok := testutil.NewFixtureClient().(*testutil.FixtureClient)
	require.True(ok)
	fixture := newE2EWorkflowClient(base)
	registry, err := ghclient.NewProviderRegistry(map[string]ghclient.Client{
		"github.com": fixture,
	})
	require.NoError(err)
	capabilities, err := registry.Capabilities(platform.KindGitHub, "github.com")
	require.NoError(err)
	assert.True(capabilities.ReadLabels)
	assert.True(capabilities.LabelMutation)

	ref := platform.RepoRef{
		Platform: platform.KindGitHub,
		Host:     "github.com",
		Owner:    "acme",
		Name:     "widgets",
		RepoPath: "acme/widgets",
	}
	catalogReader, err := registry.WorkflowCatalogReader(platform.KindGitHub, "github.com")
	require.NoError(err)
	workflows, err := catalogReader.ListManualWorkflows(t.Context(), ref)
	require.NoError(err)
	require.Len(workflows, 2)
	assert.Equal([]string{"Release", "Maintenance"}, []string{
		workflows[0].Name,
		workflows[1].Name,
	})
	assert.Equal([]platform.WorkflowInput{
		{
			Name:        "version",
			Description: "Version to publish",
			Required:    true,
			Type:        platform.WorkflowInputString,
		},
		{
			Name:       "dry_run",
			Type:       platform.WorkflowInputBoolean,
			Default:    false,
			HasDefault: true,
		},
		{
			Name:       "channel",
			Required:   true,
			Type:       platform.WorkflowInputChoice,
			Default:    "stable",
			HasDefault: true,
			Options:    []string{"stable", "beta"},
		},
		{
			Name:     "target",
			Required: true,
			Type:     platform.WorkflowInputEnvironment,
		},
	}, workflows[0].Inputs)
	assert.Empty(workflows[1].Inputs)
	environments, err := catalogReader.ListWorkflowEnvironments(t.Context(), ref)
	require.NoError(err)
	assert.Equal([]platform.WorkflowEnvironment{
		{Name: "staging"},
		{Name: "production"},
	}, environments)

	runReader, err := registry.WorkflowRunReader(platform.KindGitHub, "github.com")
	require.NoError(err)
	page, err := runReader.ListWorkflowRuns(t.Context(), ref, platform.WorkflowRunQuery{
		WorkflowID: e2eReleaseWorkflowID,
		Event:      "workflow_dispatch",
	})
	require.NoError(err)
	require.Len(page.Items, 2)
	assert.Equal([]string{"success", "failure"}, []string{
		page.Items[0].Conclusion,
		page.Items[1].Conclusion,
	})

	dispatcher, err := registry.WorkflowDispatcher(platform.KindGitHub, "github.com")
	require.NoError(err)
	submittedInputs := map[string]any{
		"version": "v2.4.0",
		"dry_run": true,
		"channel": "beta",
		"target":  "production",
	}
	result, err := dispatcher.DispatchWorkflow(t.Context(), ref, platform.WorkflowDispatchRequest{
		WorkflowID: e2eReleaseWorkflowID,
		Ref:        "feature/caching",
		Inputs:     submittedInputs,
	})
	require.NoError(err)
	require.NotNil(result.Run)
	assert.Equal(e2eDispatchedRunID, result.Run.ID)
	submittedInputs["version"] = "mutated-after-dispatch"
	assert.Equal([]e2eWorkflowDispatch{{
		Owner:      "acme",
		Repository: "widgets",
		WorkflowID: 8101,
		Ref:        "feature/caching",
		Inputs: map[string]any{
			"version": "v2.4.0",
			"dry_run": true,
			"channel": "beta",
			"target":  "production",
		},
	}}, fixture.dispatchedRequests())

	page, err = runReader.ListWorkflowRuns(t.Context(), ref, platform.WorkflowRunQuery{
		WorkflowID: e2eReleaseWorkflowID,
		Event:      "workflow_dispatch",
		Branch:     "feature/caching",
	})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal(e2eDispatchedRunID, page.Items[0].ID)
	assert.Equal("in_progress", page.Items[0].Status)
	assert.Equal("fixture-viewer", page.Items[0].Actor)
	assert.Equal("e2e-dispatched-head-sha", page.Items[0].HeadSHA)

	jobs, err := runReader.ListWorkflowRunJobs(t.Context(), ref, e2eDispatchedRunID)
	require.NoError(err)
	require.Len(jobs, 1)
	assert.Equal("publish-release", jobs[0].Name)
	require.Len(jobs[0].Steps, 2)
	assert.Equal([]string{"Prepare", "Publish"}, []string{
		jobs[0].Steps[0].Name,
		jobs[0].Steps[1].Name,
	})
	assert.Equal("in_progress", jobs[0].Steps[1].Status)
}

func TestTestTmuxTrackerRetainsFailedCleanupForRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tmux tracker probe is not supported on Windows")
	}
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state")
	failFile := filepath.Join(dir, "fail")
	command := filepath.Join(dir, "tmux-probe")
	require.NoError(os.WriteFile(command, []byte(`#!/bin/sh
case "$*" in
  *new-session*) : > "$KF_TEST_TMUX_STATE" ;;
  *kill-server*)
    [ ! -e "$KF_TEST_TMUX_FAIL" ] || exit 1
    rm -f "$KF_TEST_TMUX_STATE"
    ;;
  *list-sessions*)
    if [ -e "$KF_TEST_TMUX_STATE" ]; then
      exit 0
    fi
    echo "no server running on test socket" >&2
    exit 1
    ;;
esac
`), 0o700))
	t.Setenv("KF_TEST_TMUX_STATE", stateFile)
	t.Setenv("KF_TEST_TMUX_FAIL", failFile)
	require.NoError(os.WriteFile(failFile, nil, 0o600))

	tracker := newTestTmuxTracker()
	_, err := tracker.start([]string{command}, []string{"new-session"}, nil)
	require.NoError(err)
	require.Error(tracker.cleanup())
	require.FileExists(stateFile)
	require.NoError(os.Remove(failFile))
	require.NoError(tracker.cleanup())
	assert.NoFileExists(stateFile)
}

func TestTestTmuxTrackerRetainsUnverifiedListFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tmux tracker probe is not supported on Windows")
	}
	require := require.New(t)
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state")
	command := filepath.Join(dir, "tmux-probe")
	require.NoError(os.WriteFile(command, []byte(`#!/bin/sh
case "$*" in
  *new-session*) : > "$KF_TEST_TMUX_STATE" ;;
  *kill-server*) exit 1 ;;
  *list-sessions*) echo "permission denied" >&2; exit 2 ;;
esac
`), 0o700))
	t.Setenv("KF_TEST_TMUX_STATE", stateFile)

	tracker := newTestTmuxTracker()
	_, err := tracker.start([]string{command}, []string{"new-session"}, nil)
	require.NoError(err)
	require.Error(tracker.cleanup())
	require.FileExists(stateFile)
}

func TestWriteServerInfoFile(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "server-info.json")
	info := e2eServerInfo{
		Host:    "127.0.0.1",
		Port:    43123,
		BaseURL: "http://127.0.0.1:43123",
		PID:     4242,
	}

	require.NoError(writeServerInfoFile(path, info))

	content, err := os.ReadFile(path)
	require.NoError(err)

	var got e2eServerInfo
	require.NoError(json.Unmarshal(content, &got))
	assert := assert.New(t)
	assert.Equal(info, got)
	if runtime.GOOS != "windows" {
		fileInfo, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(os.FileMode(0o600), fileInfo.Mode().Perm())
	}
}

func TestCleanupServerInfoFileRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server-info.json")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	cleanupServerInfoFile(path)

	_, err := os.Stat(path)
	assert := assert.New(t)
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestInstanceTmuxCommandUsesPrivateSocketAndDefaultConfig(t *testing.T) {
	t.Setenv(e2eTmuxDirEnv, "")
	command := instanceTmuxCommand()

	require.Len(t, command, 5)
	assert := assert.New(t)
	assert.Equal("tmux", command[0])
	assert.Equal("-f", command[1])
	assert.Equal("/dev/null", command[2])
	assert.Equal("-L", command[3])
	assert.Regexp(`^mm-e2e-\d+-\d+-[0-9a-f]{8}$`, command[4])
}

func TestInstanceTmuxCommandUsesPlaywrightOwnedSocketDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("", "kf-e2e-tmux-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(e2eTmuxDirEnv, dir)

	command := instanceTmuxCommand()

	require.Len(t, command, 5)
	assert := assert.New(t)
	assert.Equal("tmux", command[0])
	assert.Equal("-S", command[3])
	assert.Equal(dir, filepath.Dir(command[4]))
	assert.Regexp(`^mm-e2e-\d+-\d+-[0-9a-f]{8}\.sock$`, filepath.Base(command[4]))
	assert.True(isOwnedE2ETmuxCommand(command))
}

func TestTmuxCreationGateRejectsNewSessionsAfterShutdownStarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tmux gate probe is not supported on Windows")
	}
	require := require.New(t)
	assert := assert.New(t)
	const waitTimeout = 5 * time.Second
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	logPath := filepath.Join(dir, "commands")
	command := filepath.Join(dir, "tmux-probe")
	require.NoError(os.WriteFile(command, []byte(`#!/bin/sh
case " $* " in
  *" new-session "*)
    echo new-session >> "$KF_TEST_TMUX_LOG"
    : > "$KF_TEST_TMUX_STARTED"
    while [ ! -e "$KF_TEST_TMUX_RELEASE" ]; do sleep 0.01; done
    ;;
  *" kill-server "*) echo kill-server >> "$KF_TEST_TMUX_LOG" ;;
esac
`), 0o700))
	t.Setenv("KF_TEST_TMUX_STARTED", started)
	t.Setenv("KF_TEST_TMUX_RELEASE", release)
	t.Setenv("KF_TEST_TMUX_LOG", logPath)

	gate, guarded, err := newTmuxCreationGate(dir, []string{command})
	require.NoError(err)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- procutil.Command(guarded[0], append(guarded[1:], "new-session")...).Run()
	}()
	require.Eventually(func() bool {
		_, statErr := os.Stat(started)
		return statErr == nil
	}, waitTimeout, 10*time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		gate.stop(func() {
			_ = procutil.Command(command, "kill-server").Run()
		})
		close(stopDone)
	}()
	require.Eventually(func() bool {
		_, statErr := os.Stat(filepath.Join(gate.dir, "killing"))
		return statErr == nil
	}, waitTimeout, 10*time.Millisecond)

	output, startErr := procutil.Command(
		guarded[0], append(guarded[1:], "new-session")...,
	).CombinedOutput()
	require.Error(startErr)
	assert.Contains(string(output), "e2e tmux shutdown has started")
	require.NoError(os.WriteFile(release, nil, 0o600))
	require.NoError(<-firstDone)
	select {
	case <-stopDone:
	case <-time.After(waitTimeout):
		require.Fail("tmux stop did not acquire the creation gate")
	}

	logBytes, err := os.ReadFile(logPath)
	require.NoError(err)
	assert.Equal("new-session\nkill-server\n", string(logBytes))
}

func TestPatchFixturePRSHAsUpdatesOpenPRs(t *testing.T) {
	openPR := &gh.PullRequest{
		Number: new(1),
		Head:   &gh.PullRequestBranch{},
		Base:   &gh.PullRequestBranch{},
	}
	fc := &testutil.FixtureClient{
		OpenPRs: map[string][]*gh.PullRequest{
			"acme/widgets": {openPR},
		},
	}

	patchFixturePRSHAs(fc, "acme", "widgets", 1, "head-sha", "base-sha")

	assert := assert.New(t)
	updated := fc.OpenPRs["acme/widgets"][0]
	assert.Equal("head-sha", updated.GetHead().GetSHA())
	assert.Equal("base-sha", updated.GetBase().GetSHA())
	assert.Empty(openPR.GetHead().GetSHA(), "update should replace fixture PR instead of mutating shared pointer")
}

func TestPatchFixturePRSHAsUpdatesLookupPRs(t *testing.T) {
	lookupPR := &gh.PullRequest{
		Number: new(1),
		Head:   &gh.PullRequestBranch{},
		Base:   &gh.PullRequestBranch{},
	}
	fc := &testutil.FixtureClient{
		PRs: map[string][]*gh.PullRequest{
			"acme/widgets": {lookupPR},
		},
	}

	patchFixturePRSHAs(fc, "acme", "widgets", 1, "head-sha", "base-sha")

	assert := assert.New(t)
	updated := fc.PRs["acme/widgets"][0]
	assert.Equal("head-sha", updated.GetHead().GetSHA())
	assert.Equal("base-sha", updated.GetBase().GetSHA())
	assert.Empty(lookupPR.GetHead().GetSHA(), "update should replace fixture PR instead of mutating shared pointer")
}

func TestAppStateRegistryWaitsForInFlightHandlersAfterSwap(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	oldState := &appState{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	newState := &appState{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}),
	}
	states := newAppStateRegistry(oldState)

	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		states.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}()
	<-started

	require := require.New(t)
	swapped := states.Swap(newState)
	require.Same(oldState, swapped)

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_ = swapped.waitForHandlers(context.Background())
	}()

	select {
	case <-drained:
		require.Fail("old state drained before its in-flight handler returned")
	case <-time.After(100 * time.Millisecond):
	}

	newRecorder := httptest.NewRecorder()
	states.ServeHTTP(newRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(http.StatusAccepted, newRecorder.Code)

	close(release)
	select {
	case <-oldDone:
	case <-time.After(5 * time.Second):
		require.Fail("old handler did not return")
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		require.Fail("old state did not drain after handler returned")
	}
}

func TestAppStateRegistryWaitsForAsyncStateCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newAppStateRegistry(nil)
		release := make(chan struct{})
		registry.closeAsync(func() {
			<-release
		})

		waited := make(chan struct{})
		go func() {
			registry.waitForClosers()
			close(waited)
		}()

		synctest.Wait()
		select {
		case <-waited:
			require.Fail(t, "state registry returned before async cleanup finished")
		default:
		}

		close(release)
		synctest.Wait()
		select {
		case <-waited:
		default:
			require.Fail(t, "state registry did not return after async cleanup finished")
		}
	})
}

// TestDefaultRoborevEndpointIsUnbindable pins the e2e server's
// roborev flag default to a loopback address with a privileged
// port. Reverting this default would re-introduce silent forwarding
// to a real local roborev daemon at 127.0.0.1:7373 during direct
// playwright runs.
func TestDefaultRoborevEndpointIsUnbindable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	u, err := url.Parse(defaultRoborevEndpoint)
	require.NoError(err)
	assert.Equal("127.0.0.1", u.Hostname(),
		"default roborev endpoint must be loopback")

	port, err := strconv.Atoi(u.Port())
	require.NoError(err)
	assert.Less(port, 1024,
		"default roborev port must be privileged so it cannot be "+
			"silently bound by an unrelated developer process")
	assert.Positive(port)
}

func TestBuildAppStateSeedsReviewedHeadsForUTCMergeTargets(t *testing.T) {
	assets, err := web.Assets()
	require.NoError(t, err)

	state, err := buildAppState(t.Context(), assets, appOptions{
		roborevEndpoint: defaultRoborevEndpoint,
	})
	require.NoError(t, err)
	t.Cleanup(state.close)

	for _, target := range []struct {
		owner  string
		repo   string
		number int
	}{
		{owner: "acme", repo: "widgets", number: 7},
		{owner: "acme", repo: "tools", number: 1},
	} {
		t.Run(target.repo+"-"+strconv.Itoa(target.number), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			repo, err := state.database.GetRepoByIdentity(
				t.Context(), db.GitHubRepoIdentity("github.com", target.owner, target.repo),
			)
			require.NoError(err)
			require.NotNil(repo)

			mr, err := state.database.GetMergeRequestByRepoIDAndNumber(
				t.Context(), repo.ID, target.number,
			)
			require.NoError(err)
			require.NotNil(mr)

			assert.Equal(mr.PlatformHeadSHA, mr.DiffHeadSHA)
			assert.NotEmpty(mr.DiffHeadSHA)
			assert.Equal(mr.PlatformBaseSHA, mr.DiffBaseSHA)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodGet,
				"http://127.0.0.1/api/v1/pulls/github/"+
					target.owner+"/"+target.repo+"/"+strconv.Itoa(target.number),
				nil,
			)
			state.handler.ServeHTTP(recorder, request)
			require.Equal(http.StatusOK, recorder.Code)

			var detail map[string]any
			require.NoError(json.Unmarshal(recorder.Body.Bytes(), &detail))
			assert.Equal(mr.DiffHeadSHA, detail["reviewed_head_sha"])
		})
	}
}

// TestRunDefaultRoborevFailsClosedThroughProxy is the behavioral
// guard the test review asked for: it calls run() with the default
// roborev endpoint (no -roborev override) and verifies that the
// full e2e-server startup path wires the proxy so
// /api/roborev/api/status returns a closed-fail 502 with the
// proxy's "not reachable" error JSON, instead of silently
// forwarding the probe to a real local daemon.
//
// The test exercises run() directly (not server.New) so a later
// regression anywhere in the run() wiring — config population,
// roborev endpoint propagation, proxy registration — would break
// this test. Pairs with TestDefaultRoborevEndpointIsUnbindable
// which pins the constant value itself.
func TestRunDefaultRoborevFailsClosedThroughProxy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	serverInfoFile := filepath.Join(t.TempDir(), "server-info.json")

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, 0, defaultRoborevEndpoint, serverInfoFile, "github.com", false, false)
	}()

	baseURL := waitForServerInfoBaseURL(t, serverInfoFile, done)

	resp, err := http.Get(baseURL + "/api/roborev/api/status")
	require.NoError(err)
	defer resp.Body.Close()

	assert.Equal(http.StatusBadGateway, resp.StatusCode,
		"default roborev endpoint must fail closed through the proxy")

	body, err := io.ReadAll(resp.Body)
	require.NoError(err)
	var payload map[string]string
	require.NoError(json.Unmarshal(body, &payload))
	assert.Contains(payload["error"], "roborev daemon is not reachable")

	// Cancel and confirm run() exits cleanly so the goroutine does
	// not outlive the test.
	cancel()
	select {
	case runErr := <-done:
		require.NoError(runErr)
	case <-time.After(10 * time.Second):
		require.Fail("run() did not exit within 10s of cancellation")
	}
}

// waitForServerInfoBaseURL polls the server-info file until run()
// writes it, then returns the BaseURL. Fails the test if run()
// returns early (done is closed with an error) or the file does
// not appear within the timeout.
func waitForServerInfoBaseURL(
	t *testing.T, path string, done <-chan error,
) string {
	t.Helper()
	return waitForServerInfo(t, path, done).BaseURL
}

// waitForServerInfo polls the server-info file until run() writes it,
// then returns the parsed info. Fails the test if run() returns early
// (done is closed with an error) or the file does not appear within
// the timeout.
func waitForServerInfo(
	t *testing.T, path string, done <-chan error,
) e2eServerInfo {
	t.Helper()
	r := require.New(t)
	// run() does SeedFixtures + SetupDiffRepo + stack detection before it
	// starts listening. Cold race-enabled startup can take over 30 seconds
	// when packages run concurrently in CI, so leave enough headroom for
	// resource contention while retaining a bounded readiness failure.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			r.Failf("run exited early",
				"run() exited before server-info was written: %v", err)
		default:
		}
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			var info e2eServerInfo
			if jsonErr := json.Unmarshal(data, &info); jsonErr == nil && info.BaseURL != "" {
				return info
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.FailNow("timed out waiting for server-info file")
	return e2eServerInfo{}
}

// TestRunPprofListenerFromEnv pins the contract the workspace-switch
// profiling harness depends on: KENN_FORGE_PPROF_ADDR makes run()
// start the diagnostics listener, report its resolved address in the
// server-info file, and serve the pprof index there.
func TestRunPprofListenerFromEnv(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	t.Setenv("KENN_FORGE_PPROF_ADDR", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	serverInfoFile := filepath.Join(t.TempDir(), "server-info.json")

	done := make(chan error, 1)
	go func() {
		done <- run(ctx, 0, defaultRoborevEndpoint, serverInfoFile, "github.com", false, false)
	}()

	info := waitForServerInfo(t, serverInfoFile, done)
	require.NotEmpty(info.PprofAddr,
		"server info must report the resolved pprof listener address")

	resp, err := http.Get("http://" + info.PprofAddr + "/debug/pprof/")
	require.NoError(err)
	defer resp.Body.Close()
	assert.Equal(http.StatusOK, resp.StatusCode)

	cancel()
	select {
	case runErr := <-done:
		require.NoError(runErr)
	case <-time.After(10 * time.Second):
		require.Fail("run() did not exit within 10s of cancellation")
	}
}

func TestRunCancellationStopsPrivateTmuxBeforeShutdown(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	tmuxDir, err := os.MkdirTemp("/tmp", "kf-e2e-tmux-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	t.Setenv(e2eTmuxDirEnv, tmuxDir)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	serverInfoFile := filepath.Join(t.TempDir(), "server-info.json")
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, 0, defaultRoborevEndpoint, serverInfoFile, "github.com", false, false)
	}()
	info := waitForServerInfo(t, serverInfoFile, done)
	tmuxCommand := startPrivateE2ETmuxServer(t, info.ConfigPath)

	cancel()
	select {
	case runErr := <-done:
		require.NoError(t, runErr)
	case <-time.After(10 * time.Second):
		require.Fail(t, "run() did not exit within 10s of cancellation")
	}
	requirePrivateTmuxServerStopped(t, tmuxCommand)
}

func TestE2EServerTestMainStopsPrivateTmuxOnSIGTERM(t *testing.T) {
	if os.Getenv("KENN_FORGE_E2E_SIGNAL_HELPER") == "1" {
		tmuxCommand := instanceTmuxCommand()
		args := append([]string{}, tmuxCommand[1:]...)
		args = append(args, "new-session", "-d", "-s", "signal-cleanup", "sleep 30")
		encoded, err := json.Marshal(tmuxCommand)
		if err != nil {
			os.Exit(3)
		}
		_, err = testTmux.start(tmuxCommand, args, func() {
			if writeErr := os.WriteFile(os.Getenv("KENN_FORGE_E2E_SIGNAL_READY"), encoded, 0o600); writeErr != nil {
				os.Exit(4)
			}
			time.Sleep(250 * time.Millisecond)
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		select {}
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support the tmux signal-cleanup probe")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	require := require.New(t)
	tmuxDir, err := os.MkdirTemp("/tmp", "kf-e2e-tmux-")
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	readyFile := filepath.Join(t.TempDir(), "ready.json")
	// Model the interval after WriteFile creates the ready file but before it
	// writes the JSON payload.
	require.NoError(os.WriteFile(readyFile, nil, 0o600))
	cmd := procutil.Command(os.Args[0], "-test.run=^TestE2EServerTestMainStopsPrivateTmuxOnSIGTERM$")
	cmd.Env = append(os.Environ(),
		"KENN_FORGE_E2E_SIGNAL_HELPER=1",
		"KENN_FORGE_E2E_SIGNAL_READY="+readyFile,
		e2eTmuxDirEnv+"="+tmuxDir,
	)
	require.NoError(cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		data, readErr := os.ReadFile(readyFile)
		if readErr == nil {
			var tmuxCommand []string
			if jsonErr := json.Unmarshal(data, &tmuxCommand); jsonErr == nil && len(tmuxCommand) > 0 {
				t.Cleanup(func() { killTmuxServer(tmuxCommand) })
				require.NoError(cmd.Process.Signal(syscall.SIGTERM))
				waitErr := cmd.Wait()
				var exitErr *exec.ExitError
				require.ErrorAs(waitErr, &exitErr)
				require.Equal(143, exitErr.ExitCode())
				requirePrivateTmuxServerStopped(t, tmuxCommand)
				return
			}
		}
		if time.Now().After(deadline) {
			require.FailNow("signal helper did not create its private tmux server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResetSwapsFixtureState pins the /__e2e/reset contract that the
// Playwright server pool depends on: a reset discards mutations made
// against the old state, serves the rebuilt state on the same port,
// reports the new config path, honors option overrides, and rejects
// malformed bodies instead of silently resetting to defaults.
func TestResetSwapsFixtureState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	serverInfoFile := filepath.Join(t.TempDir(), "server-info.json")
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, 0, defaultRoborevEndpoint, serverInfoFile, "github.com", false, false)
	}()
	baseURL := waitForServerInfoBaseURL(t, serverInfoFile, done)

	infoBefore := readInfoFile(t, serverInfoFile)

	seededDetail := getJSON(t, baseURL+"/api/v1/pulls/github/acme/widgets/1")
	seededMR, ok := seededDetail["merge_request"].(map[string]any)
	require.True(ok, "detail payload missing merge_request")
	seededCIStatus := seededMR["CIStatus"]

	// Mutate old-state fixture data away from the seeded value.
	resp, err := http.Post(baseURL+"/__e2e/pr-ci-state/mixed", "application/json", nil)
	require.NoError(err)
	resp.Body.Close()
	require.Equal(http.StatusOK, resp.StatusCode)
	mutatedDetail := getJSON(t, baseURL+"/api/v1/pulls/github/acme/widgets/1")
	mutatedMR, ok := mutatedDetail["merge_request"].(map[string]any)
	require.True(ok, "detail payload missing merge_request")
	require.NotEqual(seededCIStatus, mutatedMR["CIStatus"], "fixture mutation must change CI status")

	// Malformed JSON must fail loudly, not reset.
	resp, err = http.Post(baseURL+"/__e2e/reset", "application/json", strings.NewReader("{not json"))
	require.NoError(err)
	resp.Body.Close()
	assert.Equal(http.StatusBadRequest, resp.StatusCode)

	// A real reset succeeds and reports a fresh config path.
	resp, err = http.Post(baseURL+"/__e2e/reset", "application/json", nil)
	require.NoError(err)
	var resetInfo e2eServerInfo
	require.NoError(json.NewDecoder(resp.Body).Decode(&resetInfo))
	resp.Body.Close()
	require.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal(infoBefore.BaseURL, resetInfo.BaseURL, "reset must keep the listener and port")
	assert.NotEqual(infoBefore.ConfigPath, resetInfo.ConfigPath, "reset must rebuild the seeded config")

	// The mutation must not survive into the rebuilt state.
	detail := getJSON(t, baseURL+"/api/v1/pulls/github/acme/widgets/1")
	mr, ok := detail["merge_request"].(map[string]any)
	require.True(ok, "detail payload missing merge_request")
	assert.Equal(seededCIStatus, mr["CIStatus"], "seeded CI state must be restored after reset")

	// Option overrides apply to the rebuilt state.
	resp, err = http.Post(
		baseURL+"/__e2e/reset",
		"application/json",
		strings.NewReader(`{"default_platform_host":"ghe.example.com"}`),
	)
	require.NoError(err)
	resp.Body.Close()
	require.Equal(http.StatusOK, resp.StatusCode)
	repos := getJSONList(t, baseURL+"/api/v1/repos")
	foundEnterprise := false
	for _, repo := range repos {
		row, ok := repo.(map[string]any)
		if !ok {
			continue
		}
		if row["Owner"] == "enterprise" && row["Name"] == "service" {
			foundEnterprise = true
		}
	}
	assert.True(foundEnterprise, "host override must reseed the enterprise repo set")

	cancel()
	select {
	case runErr := <-done:
		require.NoError(runErr)
	case <-time.After(10 * time.Second):
		require.Fail("run() did not exit within 10s of cancellation")
	}
}

func TestResetStopsOldPrivateTmuxServerBeforeReturning(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	require := require.New(t)
	tmuxDir, err := os.MkdirTemp("/tmp", "kf-e2e-tmux-")
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(tmuxDir) })
	t.Setenv(e2eTmuxDirEnv, tmuxDir)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	serverInfoFile := filepath.Join(t.TempDir(), "server-info.json")
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, 0, defaultRoborevEndpoint, serverInfoFile, "github.com", false, false)
	}()
	info := waitForServerInfo(t, serverInfoFile, done)
	oldTmuxCommand := startPrivateE2ETmuxServer(t, info.ConfigPath)

	resp, err := http.Post(info.BaseURL+"/__e2e/reset", "application/json", nil)
	require.NoError(err)
	resp.Body.Close()
	require.Equal(http.StatusOK, resp.StatusCode)
	requirePrivateTmuxServerStopped(t, oldTmuxCommand)

	cancel()
	select {
	case runErr := <-done:
		require.NoError(runErr)
	case <-time.After(10 * time.Second):
		require.Fail("run() did not exit within 10s of cancellation")
	}
}

func startPrivateE2ETmuxServer(t *testing.T, configPath string) []string {
	t.Helper()
	cfg, err := config.Load(configPath)
	require.NoError(t, err)
	tmuxCommand := cfg.TmuxCommand()
	require.True(t, isOwnedE2ETmuxCommand(tmuxCommand))
	args := append([]string{}, tmuxCommand[1:]...)
	args = append(args, "new-session", "-d", "-s", "forge-e2e-shutdown-test", "sleep 30")
	stop, err := testTmux.start(tmuxCommand, args, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, stop())
	})
	return testTmuxCommandIdentity(tmuxCommand)
}

func requirePrivateTmuxServerStopped(t *testing.T, tmuxCommand []string) {
	t.Helper()
	args := append([]string{}, tmuxCommand[1:]...)
	args = append(args, "list-sessions")
	output, err := procutil.Command(tmuxCommand[0], args...).CombinedOutput()
	require.Error(t, err, "private tmux server still accepts commands: %s", output)
}

func readInfoFile(t *testing.T, path string) e2eServerInfo {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var info e2eServerInfo
	require.NoError(t, json.Unmarshal(data, &info))
	return info
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload
}

func getJSONList(t *testing.T, url string) []any {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload []any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload
}
