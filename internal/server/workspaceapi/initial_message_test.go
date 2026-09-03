package workspaceapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	ptyownerruntime "go.kenn.io/forge/internal/ptyowner/runtime"
	"go.kenn.io/forge/internal/ptysize"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

func TestNormalizeInitialAgentMessage(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		want      string
		wantBytes int
		wantErr   string
	}{
		{name: "line endings", message: "first\r\nsecond\rthird", want: "first\nsecond\nthird", wantBytes: 18},
		{name: "tab", message: "review\tthis", wantErr: "control character"},
		{name: "vertical tab", message: "review\vthis", wantErr: "control character"},
		{name: "form feed", message: "review\fthis", wantErr: "control character"},
		{name: "next line", message: "review\u0085this", wantErr: "control character"},
		{name: "maximum", message: strings.Repeat("a", 64<<10), wantBytes: 64 << 10},
		{name: "blank", message: " \n\t ", wantErr: "must not be blank"},
		{name: "invalid utf8", message: string([]byte{0xff}), wantErr: "valid UTF-8"},
		{name: "nul", message: "before\x00after", wantErr: "control character"},
		{name: "escape", message: "before\x1bafter", wantErr: "control character"},
		{name: "oversized", message: strings.Repeat("a", (64<<10)+1), wantErr: "64 KiB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			normalized, messageBytes, err := normalizeInitialAgentMessage(tc.message)
			if tc.wantErr != "" {
				require.ErrorContains(err, tc.wantErr)
				return
			}
			require.NoError(err)
			if tc.want != "" {
				assert.Equal(tc.want, normalized)
			}
			assert.Equal(tc.wantBytes, messageBytes)
		})
	}
}

func TestInitialMessageSubmitFailureReleasesPreWriteAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := New(Deps{Now: func() time.Time {
		return time.Date(2026, 8, 18, 17, 0, 0, 0, time.UTC)
	}})
	proposed := initialMessageAttempt{
		TargetKey: "codex", Message: "review this",
	}
	_, reserved := handler.reserveInitialMessageAttempt("ws-1", "runtime-1", proposed)
	require.True(reserved)

	result, err := handler.handleInitialMessageSubmitError(
		"ws-1", "runtime-1", proposed, localruntime.ErrInitialMessageNotWritten,
	)

	require.Error(err)
	assert.Empty(result.State)
	var problem *httpapi.ProblemError
	require.ErrorAs(err, &problem)
	assert.Equal(http.StatusConflict, problem.Status)
	_, found := handler.initialMessageAttempt("ws-1", "runtime-1")
	assert.False(found)
}

func TestInitialMessageInactivePasteModeReturnsRetryableServiceSignal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := New(Deps{Now: func() time.Time {
		return time.Date(2026, 8, 18, 17, 0, 0, 0, time.UTC)
	}})
	proposed := initialMessageAttempt{
		TargetKey: "codex", Message: "first\nsecond",
	}
	_, reserved := handler.reserveInitialMessageAttempt("ws-1", "runtime-1", proposed)
	require.True(reserved)

	result, err := handler.handleInitialMessageSubmitError(
		"ws-1", "runtime-1", proposed, localruntime.ErrBracketedPasteInactive,
	)

	require.ErrorIs(err, ErrInitialMessageInputModeNotReady)
	assert.Empty(result.State)
	_, found := handler.initialMessageAttempt("ws-1", "runtime-1")
	assert.False(found)
}

func TestSubmitInitialMessageServiceReturnsDeliveredStateAndRoutesShareAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	database := dbtest.Open(t)
	worktree := t.TempDir()
	workspaceID := "ws-initial-message"
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID: workspaceID, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widgets",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
		GitHeadRef: "feature/message", WorkspaceBranch: "feature/message",
		WorktreePath: worktree, TmuxSession: "forge-initial-message", Status: "ready",
	}))

	owner := newInitialMessagePTYOwner()
	runtime := localruntime.NewManager(localruntime.Options{
		Targets: []localruntime.LaunchTarget{{
			Key: "codex", Label: "Codex", Kind: localruntime.LaunchTargetAgent,
			Source: "test", Command: []string{"unused"}, Available: true,
		}},
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(runtime.Shutdown)
	session, err := runtime.Launch(ctx, workspaceID, worktree, "codex")
	require.NoError(err)
	require.NoError(database.UpsertWorkspaceRuntimeSession(ctx, &db.WorkspaceRuntimeSession{
		WorkspaceID: workspaceID, SessionKey: session.Key,
		TargetKey: "codex", Label: "Codex", Kind: "agent", Scope: "session",
		CreatedAt: session.CreatedAt,
	}))
	activity := agentactivity.NewStore(t.TempDir())
	require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
		SessionID: "coding-session", CWD: worktree,
		HookEventName: "UserPromptSubmit",
	}, session.Key))

	handler := New(Deps{
		DB: database, Workspaces: workspace.NewManager(database, t.TempDir()),
		Runtime: runtime, AgentActivity: activity,
	})
	mux := http.NewServeMux()
	api := humago.NewWithPrefix(mux, "/api/v1", huma.DefaultConfig("initial message test", "1"))
	handler.Register(api)
	endpoint := "/api/v1/workspaces/" + workspaceID +
		"/runtime/sessions/" + session.Key + "/initial-message"
	postContext := func(requestContext context.Context, target, targetKey, message string) *httptest.ResponseRecorder {
		t.Helper()
		body, marshalErr := json.Marshal(map[string]string{
			"target_key": targetKey, "message": message,
		})
		require.NoError(marshalErr)
		request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)).WithContext(requestContext)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	post := func(target, targetKey, message string) *httptest.ResponseRecorder {
		t.Helper()
		return postContext(ctx, target, targetKey, message)
	}

	requestContext, cancelRequest := context.WithCancel(ctx)
	owner.pty.setOnWrite(cancelRequest)
	var serviceStatus InitialMessageResult
	require.Eventually(func() bool {
		var submitErr error
		serviceStatus, submitErr = handler.SubmitInitialMessageService(
			requestContext,
			InitialMessageRequest{
				WorkspaceID: workspaceID, RuntimeSessionKey: session.Key,
				TargetKey: "CoDeX", Message: "review this",
			},
		)
		if errors.Is(submitErr, ErrInitialMessageInputModeNotReady) {
			return false
		}
		require.NoError(submitErr)
		return true
	}, time.Second, 10*time.Millisecond)
	owner.pty.setOnWrite(nil)
	assert.Equal(initialMessageDelivered, serviceStatus.State)
	assert.Equal(11, serviceStatus.MessageBytes)
	require.NotNil(serviceStatus.DeliveredAt)
	assert.Equal("\x1b[200~review this\x1b[201~\r", string(owner.pty.written()))

	response := post(endpoint, "codex", "review this")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var messageStatus struct {
		TargetKey    string     `json:"target_key"`
		State        string     `json:"state"`
		MessageBytes int        `json:"message_bytes"`
		DeliveredAt  *time.Time `json:"delivered_at"`
	}
	require.NoError(json.NewDecoder(response.Body).Decode(&messageStatus))
	assert.Equal("codex", messageStatus.TargetKey)
	assert.Equal(initialMessageDelivered, messageStatus.State)
	assert.Equal(11, messageStatus.MessageBytes)
	require.NotNil(messageStatus.DeliveredAt)
	assert.Equal(time.UTC, messageStatus.DeliveredAt.Location())
	assert.Equal("\x1b[200~review this\x1b[201~\r", string(owner.pty.written()))

	response = post(endpoint, "codex", "review this")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal("\x1b[200~review this\x1b[201~\r", string(owner.pty.written()))

	response = post(endpoint, "codex", "review that")
	require.Equal(http.StatusConflict, response.Code, response.Body.String())
	assert.Equal("\x1b[200~review this\x1b[201~\r", string(owner.pty.written()))

	followUp, err := handler.SubmitAgentMessageService(
		ctx, workspaceID, session.Key, "keep going",
	)
	require.NoError(err)
	assert.Equal("codex", followUp.TargetKey)
	assert.Equal(10, followUp.MessageBytes)
	assert.Equal(
		"\x1b[200~review this\x1b[201~\r\x1b[200~keep going\x1b[201~\r",
		string(owner.pty.written()),
	)

	launchSession := func(codingSession string, report bool) localruntime.SessionInfo {
		t.Helper()
		launched, launchErr := runtime.Launch(ctx, workspaceID, worktree, "codex")
		require.NoError(launchErr)
		require.NoError(database.UpsertWorkspaceRuntimeSession(ctx, &db.WorkspaceRuntimeSession{
			WorkspaceID: workspaceID, SessionKey: launched.Key,
			TargetKey: "codex", Label: "Codex", Kind: "agent", Scope: "session",
			CreatedAt: launched.CreatedAt,
		}))
		if report {
			require.NoError(activity.HandleEvent("codex", agentactivity.HookEvent{
				SessionID: codingSession, CWD: worktree,
				HookEventName: "UserPromptSubmit",
			}, launched.Key))
		}
		return launched
	}
	endpointFor := func(runtimeKey string) string {
		return "/api/v1/workspaces/" + workspaceID +
			"/runtime/sessions/" + runtimeKey + "/initial-message"
	}

	failingSession := launchSession("coding-failure", true)
	owner.pty.setWriteError(errors.New("write failed"))
	failedResult, err := handler.SubmitInitialMessageService(ctx, InitialMessageRequest{
		WorkspaceID: workspaceID, RuntimeSessionKey: failingSession.Key,
		TargetKey: "codex", Message: "try once",
	})
	require.Error(err)
	assert.Equal(initialMessageUncertain, failedResult.State)
	failedAttempt, found := handler.initialMessageAttempt(workspaceID, failingSession.Key)
	require.True(found)
	assert.Equal(initialMessageUncertain, failedAttempt.State)
	owner.pty.setWriteError(nil)

	owner.setEmitBracketedPaste(false)
	inactivePasteSession := launchSession("coding-multiline", true)
	writtenBefore := owner.pty.written()
	response = post(
		endpointFor(inactivePasteSession.Key), "codex", "first\nsecond",
	)
	require.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(writtenBefore, owner.pty.written())
	_, found = handler.initialMessageAttempt(workspaceID, inactivePasteSession.Key)
	assert.False(found)

	owner.setEmitBracketedPaste(true)
	unreportedSession := launchSession("coding-unreported", false)
	response = post(
		endpointFor(unreportedSession.Key), "codex", "do not send",
	)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	_, found = handler.initialMessageAttempt(workspaceID, unreportedSession.Key)
	assert.True(found)

	require.NoError(runtime.Stop(ctx, workspaceID, session.Key))
	require.NoError(database.DeleteWorkspaceRuntimeSession(ctx, workspaceID, session.Key))
	response = post(endpoint, "claude", "review this")
	require.Equal(http.StatusConflict, response.Code, response.Body.String())

	getRecorder := httptest.NewRecorder()
	mux.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, endpoint, nil))
	require.Equal(http.StatusOK, getRecorder.Code, getRecorder.Body.String())
	messageStatus = struct {
		TargetKey    string     `json:"target_key"`
		State        string     `json:"state"`
		MessageBytes int        `json:"message_bytes"`
		DeliveredAt  *time.Time `json:"delivered_at"`
	}{}
	require.NoError(json.NewDecoder(getRecorder.Body).Decode(&messageStatus))
	assert.Equal("codex", messageStatus.TargetKey)
	assert.Equal(initialMessageDelivered, messageStatus.State)
}

type initialMessagePTYOwner struct {
	mu                 sync.Mutex
	pty                *initialMessagePTY
	ptys               map[string]*initialMessagePTY
	emitBracketedPaste bool
}

func newInitialMessagePTYOwner() *initialMessagePTYOwner {
	return &initialMessagePTYOwner{
		ptys: make(map[string]*initialMessagePTY), emitBracketedPaste: true,
	}
}

func (o *initialMessagePTYOwner) HasState(session string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ptys[session] != nil
}

func (o *initialMessagePTYOwner) Attach(
	_ context.Context,
	session string,
) (ptyownerruntime.PTY, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ptys[session], nil
}

func (o *initialMessagePTYOwner) Start(
	_ context.Context,
	session string,
	_ string,
	_ []string,
	_ []string,
	_ map[string]string,
) (ptyownerruntime.PTY, error) {
	pty := &initialMessagePTY{
		output: make(chan []byte, 8), done: make(chan struct{}),
	}
	o.mu.Lock()
	o.pty = pty
	o.ptys[session] = pty
	emitBracketedPaste := o.emitBracketedPaste
	o.mu.Unlock()
	if emitBracketedPaste {
		pty.output <- []byte("\x1b[?2004h")
	}
	return pty, nil
}

func (o *initialMessagePTYOwner) Stop(_ context.Context, session string) error {
	o.mu.Lock()
	pty := o.ptys[session]
	delete(o.ptys, session)
	o.mu.Unlock()
	if pty != nil {
		pty.Close()
	}
	return nil
}

func (o *initialMessagePTYOwner) setEmitBracketedPaste(enabled bool) {
	o.mu.Lock()
	o.emitBracketedPaste = enabled
	o.mu.Unlock()
}

type initialMessagePTY struct {
	mu       sync.Mutex
	output   chan []byte
	done     chan struct{}
	writes   []byte
	writeErr error
	onWrite  func()
	once     sync.Once
}

func (p *initialMessagePTY) Output() <-chan []byte         { return p.output }
func (p *initialMessagePTY) Done() <-chan struct{}         { return p.done }
func (p *initialMessagePTY) ExitCode() int                 { return 0 }
func (p *initialMessagePTY) Resize(ptysize.Geometry) error { return nil }

func (p *initialMessagePTY) Write(data []byte) error {
	p.mu.Lock()
	if p.writeErr != nil {
		p.mu.Unlock()
		return p.writeErr
	}
	p.writes = append(p.writes, data...)
	onWrite := p.onWrite
	output := p.output
	p.mu.Unlock()
	output <- bytes.Clone(data)
	if onWrite != nil {
		onWrite()
	}
	return nil
}

func (p *initialMessagePTY) setWriteError(err error) {
	p.mu.Lock()
	p.writeErr = err
	p.mu.Unlock()
}

func (p *initialMessagePTY) setOnWrite(onWrite func()) {
	p.mu.Lock()
	p.onWrite = onWrite
	p.mu.Unlock()
}

func (p *initialMessagePTY) written() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes.Clone(p.writes)
}

func (p *initialMessagePTY) Close() {
	p.once.Do(func() {
		close(p.output)
		close(p.done)
	})
}
