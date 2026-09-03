package workspaceapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

const maxInitialAgentMessageBytes = 64 << 10

const (
	initialMessagePending   = "pending"
	initialMessageDelivered = "delivered"
	initialMessageUncertain = "uncertain"
)

var ErrInitialMessageInputModeNotReady = errors.New("agent terminal input mode is not ready")

type initialMessageKey struct {
	workspaceID       string
	runtimeSessionKey string
}

// initialMessageAttempt is deliberately process-local. The normalized prompt
// is retained only so retries against this daemon can be compared without
// exposing prompt content through the API. Daemon restart clears all attempts.
type initialMessageAttempt struct {
	TargetKey   string
	Message     string
	State       string
	ReservedAt  time.Time
	DeliveredAt *time.Time
}

type initialMessagePathInput struct {
	ID         string `path:"id"`
	SessionKey string `path:"session_key"`
}

type submitInitialMessageInput struct {
	ID         string `path:"id"`
	SessionKey string `path:"session_key"`
	Body       struct {
		TargetKey string `json:"target_key"`
		Message   string `json:"message"`
	}
}

type initialMessageOutput struct {
	Body agentInitialMessageStatusResponse
}

func normalizeInitialAgentMessage(message string) (string, int, error) {
	if !utf8.ValidString(message) {
		return "", 0, errors.New("message must be valid UTF-8")
	}
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\n")
	if strings.TrimSpace(message) == "" {
		return "", 0, errors.New("message must not be blank")
	}
	for _, value := range message {
		if value != '\n' && !unicode.IsPrint(value) {
			return "", 0, fmt.Errorf("message contains unsafe control character U+%04X", value)
		}
	}
	messageBytes := len(message)
	if messageBytes > maxInitialAgentMessageBytes {
		return "", 0, errors.New("message must not exceed 64 KiB after line-ending normalization")
	}
	return message, messageBytes, nil
}

func (s *Handler) getInitialMessageStatus(
	ctx context.Context,
	input *initialMessagePathInput,
) (*initialMessageOutput, error) {
	result, err := s.GetInitialMessageService(ctx, input.ID, input.SessionKey)
	if err != nil {
		return nil, err
	}
	return &initialMessageOutput{Body: *initialMessageResultResponse(result)}, nil
}

func (s *Handler) submitInitialMessage(
	ctx context.Context,
	input *submitInitialMessageInput,
) (*initialMessageOutput, error) {
	result, err := s.SubmitInitialMessageService(ctx, InitialMessageRequest{
		WorkspaceID: input.ID, RuntimeSessionKey: input.SessionKey,
		TargetKey: input.Body.TargetKey, Message: input.Body.Message,
	})
	if errors.Is(err, ErrInitialMessageInputModeNotReady) {
		return nil, httpapi.Validation("body.message", err.Error())
	}
	if err != nil {
		return nil, err
	}
	return &initialMessageOutput{Body: *initialMessageResultResponse(result)}, nil
}

func (s *Handler) GetInitialMessageService(
	_ context.Context, workspaceID, runtimeSessionKey string,
) (InitialMessageResult, error) {
	attempt, ok := s.initialMessageAttempt(workspaceID, runtimeSessionKey)
	if !ok {
		return InitialMessageResult{}, httpapi.NotFound(
			httpapi.CodeNotFound, "initial message status not found", nil,
		)
	}
	return initialMessageAttemptResult(attempt), nil
}

func (s *Handler) SubmitInitialMessageService(
	ctx context.Context, req InitialMessageRequest,
) (InitialMessageResult, error) {
	if s.workspaces == nil || s.runtime == nil {
		return InitialMessageResult{}, httpapi.ServiceUnavailable("initial message delivery not configured")
	}
	targetKey := strings.ToLower(strings.TrimSpace(req.TargetKey))
	if targetKey == "" {
		return InitialMessageResult{}, httpapi.Validation("body.target_key", "target_key is required")
	}
	message, _, err := normalizeInitialAgentMessage(req.Message)
	if err != nil {
		return InitialMessageResult{}, httpapi.Validation("body.message", err.Error())
	}
	proposed := initialMessageAttempt{TargetKey: targetKey, Message: message}
	if existing, ok := s.initialMessageAttempt(req.WorkspaceID, req.RuntimeSessionKey); ok {
		return existingInitialMessageAttemptResult(existing, proposed)
	}
	summary, err := s.workspaces.GetSummary(ctx, req.WorkspaceID)
	if err != nil {
		return InitialMessageResult{}, httpapi.Internal("get workspace failed")
	}
	if summary == nil {
		return InitialMessageResult{}, httpapi.NotFound(
			httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
		)
	}
	liveTarget := ""
	for _, runtimeSession := range s.runtime.ListSessions(req.WorkspaceID) {
		if runtimeSession.Key == req.RuntimeSessionKey &&
			runtimeSession.Kind == localruntime.LaunchTargetAgent &&
			(runtimeSession.Status == localruntime.SessionStatusStarting ||
				runtimeSession.Status == localruntime.SessionStatusRunning) {
			liveTarget = strings.ToLower(strings.TrimSpace(runtimeSession.TargetKey))
			break
		}
	}
	if liveTarget == "" {
		return InitialMessageResult{}, httpapi.Conflict(
			httpapi.CodeConflict, "agent runtime session is not live", nil,
		)
	}
	if liveTarget != targetKey {
		return InitialMessageResult{}, httpapi.Conflict(
			httpapi.CodeConflict, "target_key does not match the live agent runtime", nil,
		)
	}
	attempt, reserved := s.reserveInitialMessageAttempt(req.WorkspaceID, req.RuntimeSessionKey, proposed)
	if !reserved {
		return existingInitialMessageAttemptResult(attempt, proposed)
	}
	if err := s.runtime.SubmitInitialMessage(ctx, req.WorkspaceID, req.RuntimeSessionKey, message); err != nil {
		return s.handleInitialMessageSubmitError(
			req.WorkspaceID, req.RuntimeSessionKey, proposed, err,
		)
	}
	delivered := s.finishInitialMessageAttempt(
		req.WorkspaceID, req.RuntimeSessionKey, initialMessageDelivered,
	)
	return initialMessageAttemptResult(delivered), nil
}

func (s *Handler) SubmitAgentMessageService(
	ctx context.Context, workspaceID, runtimeSessionKey, message string,
) (AgentMessageResult, error) {
	if s.runtime == nil {
		return AgentMessageResult{}, httpapi.ServiceUnavailable("agent message delivery not configured")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return AgentMessageResult{}, httpapi.Validation("workspace_id", "workspace_id is required")
	}
	runtimeSessionKey = strings.TrimSpace(runtimeSessionKey)
	if runtimeSessionKey == "" {
		return AgentMessageResult{}, httpapi.Validation(
			"runtime_session_key", "runtime_session_key is required",
		)
	}
	message, messageBytes, err := normalizeInitialAgentMessage(message)
	if err != nil {
		return AgentMessageResult{}, httpapi.Validation("message", err.Error())
	}
	targetKey := ""
	for _, session := range s.runtime.ListSessions(workspaceID) {
		if session.Key == runtimeSessionKey && session.Kind == localruntime.LaunchTargetAgent &&
			(session.Status == localruntime.SessionStatusStarting ||
				session.Status == localruntime.SessionStatusRunning) {
			targetKey = session.TargetKey
			break
		}
	}
	if targetKey == "" {
		return AgentMessageResult{}, httpapi.Conflict(
			httpapi.CodeConflict, "agent runtime session is not live", nil,
		)
	}
	if err := s.runtime.SubmitAgentMessage(ctx, workspaceID, runtimeSessionKey, message); err != nil {
		if errors.Is(err, localruntime.ErrBracketedPasteInactive) {
			return AgentMessageResult{}, ErrInitialMessageInputModeNotReady
		}
		if errors.Is(err, localruntime.ErrInitialMessageNotWritten) {
			return AgentMessageResult{}, httpapi.Conflict(httpapi.CodeConflict, err.Error(), nil)
		}
		return AgentMessageResult{}, httpapi.Internal("submit agent message failed")
	}
	return AgentMessageResult{
		TargetKey: targetKey, MessageBytes: messageBytes, SubmittedAt: s.now().UTC(),
	}, nil
}

func (s *Handler) handleInitialMessageSubmitError(
	workspaceID string,
	runtimeSessionKey string,
	proposed initialMessageAttempt,
	err error,
) (InitialMessageResult, error) {
	if errors.Is(err, localruntime.ErrBracketedPasteInactive) {
		s.releaseInitialMessageAttempt(workspaceID, runtimeSessionKey, proposed)
		return InitialMessageResult{}, ErrInitialMessageInputModeNotReady
	}
	if errors.Is(err, localruntime.ErrInitialMessageNotWritten) {
		s.releaseInitialMessageAttempt(workspaceID, runtimeSessionKey, proposed)
		return InitialMessageResult{}, httpapi.Conflict(
			httpapi.CodeConflict, err.Error(), nil,
		)
	}
	uncertain := s.finishInitialMessageAttempt(
		workspaceID, runtimeSessionKey, initialMessageUncertain,
	)
	return initialMessageAttemptResult(uncertain), httpapi.Internal("submit initial message failed")
}

func existingInitialMessageAttemptResult(
	existing initialMessageAttempt,
	proposed initialMessageAttempt,
) (InitialMessageResult, error) {
	if existing.TargetKey != proposed.TargetKey ||
		existing.Message != proposed.Message {
		return initialMessageAttemptConflict(existing)
	}
	return initialMessageAttemptResult(existing), nil
}

func initialMessageAttemptConflict(
	existing initialMessageAttempt,
) (InitialMessageResult, error) {
	return InitialMessageResult{}, httpapi.Conflict(
		httpapi.CodeConflict,
		"an initial message attempt already exists for this runtime session",
		map[string]any{
			"target_key":    existing.TargetKey,
			"message_bytes": len(existing.Message),
			"state":         existing.State,
		},
	)
}

func (s *Handler) initialMessageAttempt(
	workspaceID string,
	runtimeSessionKey string,
) (initialMessageAttempt, bool) {
	s.initialMessagesMu.Lock()
	defer s.initialMessagesMu.Unlock()
	attempt, ok := s.initialMessages[initialMessageKey{
		workspaceID: workspaceID, runtimeSessionKey: runtimeSessionKey,
	}]
	return attempt, ok
}

func (s *Handler) reserveInitialMessageAttempt(
	workspaceID string,
	runtimeSessionKey string,
	proposed initialMessageAttempt,
) (initialMessageAttempt, bool) {
	s.initialMessagesMu.Lock()
	defer s.initialMessagesMu.Unlock()
	if s.initialMessages == nil {
		s.initialMessages = make(map[initialMessageKey]initialMessageAttempt)
	}
	key := initialMessageKey{
		workspaceID: workspaceID, runtimeSessionKey: runtimeSessionKey,
	}
	if existing, ok := s.initialMessages[key]; ok {
		return existing, false
	}
	proposed.State = initialMessagePending
	proposed.ReservedAt = s.now().UTC()
	s.initialMessages[key] = proposed
	return proposed, true
}

func (s *Handler) releaseInitialMessageAttempt(
	workspaceID string,
	runtimeSessionKey string,
	proposed initialMessageAttempt,
) {
	s.initialMessagesMu.Lock()
	defer s.initialMessagesMu.Unlock()
	key := initialMessageKey{
		workspaceID: workspaceID, runtimeSessionKey: runtimeSessionKey,
	}
	existing, ok := s.initialMessages[key]
	if ok && existing.State == initialMessagePending &&
		existing.TargetKey == proposed.TargetKey &&
		existing.Message == proposed.Message {
		delete(s.initialMessages, key)
	}
}

func (s *Handler) finishInitialMessageAttempt(
	workspaceID string,
	runtimeSessionKey string,
	state string,
) initialMessageAttempt {
	s.initialMessagesMu.Lock()
	defer s.initialMessagesMu.Unlock()
	key := initialMessageKey{
		workspaceID: workspaceID, runtimeSessionKey: runtimeSessionKey,
	}
	attempt := s.initialMessages[key]
	attempt.State = state
	if state == initialMessageDelivered {
		deliveredAt := s.now().UTC()
		attempt.DeliveredAt = &deliveredAt
	}
	s.initialMessages[key] = attempt
	return attempt
}

func (s *Handler) clearInitialMessageAttempts(workspaceID string) {
	s.initialMessagesMu.Lock()
	defer s.initialMessagesMu.Unlock()
	for key := range s.initialMessages {
		if key.workspaceID == workspaceID {
			delete(s.initialMessages, key)
		}
	}
}
