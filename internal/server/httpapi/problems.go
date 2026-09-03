// Package httpapi defines the shared HTTP response contract used by the
// server's independently registered API domains.
//
// Problems are returned as an RFC 9457 (application/problem+json) error
// envelope. The
// envelope adds two top-level fields beyond the huma defaults so frontend
// code can branch on stable, machine-readable signals instead of substring-
// matching English prose:
//
//   - `code`: a camelCase string from the closed enum below.
//   - `details`: a free-form map carrying machine-readable context.
//
// Construction goes through the problem* helpers; package init replaces
// huma.NewError so legacy huma.Error4xx/Error5xx callers (which we migrate
// away from in the same change set) still produce a valid envelope with
// a status-derived code while migration is in flight.
package httpapi

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/tokenauth"
)

// ProblemCode is the machine-readable error code carried on the wire.
type ProblemCode string

// The closed set of wire codes. Order matters: the enum struct tag on
// ProblemError.Code must list these in the same order, and the
// allProblemCodes test asserts the two stay in sync.
const (
	CodeBadRequest                    ProblemCode = "badRequest"
	CodeBranchConflict                ProblemCode = "branchConflict"
	CodeBranchInUse                   ProblemCode = "branchInUse"
	CodeBranchProtected               ProblemCode = "branchProtected"
	CodeCommentNotFound               ProblemCode = "commentNotFound"
	CodeConflict                      ProblemCode = "conflict"
	CodeDestinationExists             ProblemCode = "destinationExists"
	CodeForbidden                     ProblemCode = "forbidden"
	CodeGitCredentialUnavailable      ProblemCode = "gitCredentialUnavailable"
	CodeHookFailed                    ProblemCode = "hookFailed"
	CodeHubUnavailable                ProblemCode = "hubUnavailable"
	CodeInternalError                 ProblemCode = "internalError"
	CodeIssueNotFound                 ProblemCode = "issueNotFound"
	CodeMutationOutcomeUnknown        ProblemCode = "mutationOutcomeUnknown"
	CodeNotFound                      ProblemCode = "notFound"
	CodePayloadTooLarge               ProblemCode = "payloadTooLarge"
	CodeProjectNotFound               ProblemCode = "projectNotFound"
	CodePullNotFound                  ProblemCode = "pullNotFound"
	CodeRateLimited                   ProblemCode = "rateLimited"
	CodeRepoNotFound                  ProblemCode = "repoNotFound"
	CodeResyncRequired                ProblemCode = "resyncRequired"
	CodeServiceUnavailable            ProblemCode = "serviceUnavailable"
	CodeSettingsUnavailable           ProblemCode = "settingsUnavailable"
	CodeSpokePreparationInProgress    ProblemCode = "spokePreparationInProgress"
	CodeToolMissing                   ProblemCode = "toolMissing"
	CodeToolUnauthenticated           ProblemCode = "toolUnauthenticated"
	CodeUnauthorized                  ProblemCode = "unauthorized"
	CodeUnsupportedCapability         ProblemCode = "unsupportedCapability"
	CodeUpstreamError                 ProblemCode = "upstreamError"
	CodeValidationError               ProblemCode = "validationError"
	CodeWorkspaceAlreadyExists        ProblemCode = "workspaceAlreadyExists"
	CodeWorkspaceDeletionInProgress   ProblemCode = "workspaceDeletionInProgress"
	CodeWorkspaceDirectoryNotReusable ProblemCode = "workspaceDirectoryNotReusable"
	CodeWorkspaceNotFound             ProblemCode = "workspaceNotFound"
	CodeWorkspaceSetupInProgress      ProblemCode = "workspaceSetupInProgress"
	CodeWorktreeDirty                 ProblemCode = "worktreeDirty"
)

// allProblemCodes returns every declared ProblemCode in alphabetical order.
// The enum tag on ProblemError.Code must list the same set in the same
// order; problems_test.go asserts the two are consistent.
func allProblemCodes() []ProblemCode {
	return []ProblemCode{
		CodeBadRequest,
		CodeBranchConflict,
		CodeBranchInUse,
		CodeBranchProtected,
		CodeCommentNotFound,
		CodeConflict,
		CodeDestinationExists,
		CodeForbidden,
		CodeGitCredentialUnavailable,
		CodeHookFailed,
		CodeHubUnavailable,
		CodeInternalError,
		CodeIssueNotFound,
		CodeMutationOutcomeUnknown,
		CodeNotFound,
		CodePayloadTooLarge,
		CodeProjectNotFound,
		CodePullNotFound,
		CodeRateLimited,
		CodeRepoNotFound,
		CodeResyncRequired,
		CodeServiceUnavailable,
		CodeSettingsUnavailable,
		CodeSpokePreparationInProgress,
		CodeToolMissing,
		CodeToolUnauthenticated,
		CodeUnauthorized,
		CodeUnsupportedCapability,
		CodeUpstreamError,
		CodeValidationError,
		CodeWorkspaceAlreadyExists,
		CodeWorkspaceDeletionInProgress,
		CodeWorkspaceDirectoryNotReusable,
		CodeWorkspaceNotFound,
		CodeWorkspaceSetupInProgress,
		CodeWorktreeDirty,
	}
}

// ProblemError is the RFC 9457 problem-details envelope returned for every
// failure path. The huma.ErrorModel-compatible fields (Type/Title/Status/
// Detail/Instance/Errors) keep behavior parity with huma's defaults so
// existing clients keep working; the Code and Details extension members
// are new.
//
// The Go type name "ProblemError" intentionally differs from huma's
// "ErrorModel" to avoid shadowing the upstream type. The OpenAPI schema
// name therefore becomes ProblemError too, and generated clients pick
// up that symbol (components["schemas"]["ProblemError"] in TS,
// apiclient.ProblemError in Go).
type ProblemError struct {
	// Type is a URI reference identifying the problem type. Defaults to
	// "about:blank" per RFC 9457 when not set.
	Type string `json:"type,omitempty" format:"uri" default:"about:blank" example:"https://example.com/errors/example" doc:"A URI reference to human-readable documentation for the error."`

	// Title is a short, human-readable summary. Stable across occurrences
	// of the same problem type.
	Title string `json:"title,omitempty" example:"Bad Request" doc:"A short, human-readable summary of the problem type. This value should not change between occurrences of the error."`

	// Status is the HTTP status code. Always set by the helpers.
	Status int `json:"status,omitempty" example:"400" doc:"HTTP status code"`

	// Detail is a human-readable explanation of this occurrence.
	Detail string `json:"detail,omitempty" example:"Property foo is required but is missing." doc:"A human-readable explanation specific to this occurrence of the problem."`

	// Instance is a URI reference identifying this specific occurrence.
	Instance string `json:"instance,omitempty" format:"uri" example:"https://example.com/error-log/abc123" doc:"A URI reference that identifies the specific occurrence of the problem."`

	// Errors keeps parity with huma.ErrorModel for the validation-failure
	// path where huma emits per-field details. New code paths should
	// populate Details instead and leave Errors empty.
	Errors []*huma.ErrorDetail `json:"errors,omitempty" doc:"Optional list of individual error details"`

	// Code is the machine-readable error code drawn from the closed enum
	// in allProblemCodes(). Frontend logic branches on this value.
	Code ProblemCode `json:"code" enum:"badRequest,branchConflict,branchInUse,branchProtected,commentNotFound,conflict,destinationExists,forbidden,gitCredentialUnavailable,hookFailed,hubUnavailable,internalError,issueNotFound,mutationOutcomeUnknown,notFound,payloadTooLarge,projectNotFound,pullNotFound,rateLimited,repoNotFound,resyncRequired,serviceUnavailable,settingsUnavailable,spokePreparationInProgress,toolMissing,toolUnauthenticated,unauthorized,unsupportedCapability,upstreamError,validationError,workspaceAlreadyExists,workspaceDeletionInProgress,workspaceDirectoryNotReusable,workspaceNotFound,workspaceSetupInProgress,worktreeDirty" example:"badRequest" doc:"Machine-readable error code. Stable across occurrences."`

	// Details is a free-form map of machine-readable context for this
	// occurrence (e.g. {capability: "merge_mutation"} or
	// {retryAfter: "2026-05-19T12:00:00Z"}).
	Details map[string]any `json:"details,omitempty" doc:"Machine-readable error context, keyed by code-specific conventions."`
}

// Error returns Detail (or the code if Detail is empty) so ProblemError
// satisfies the error interface.
func (p *ProblemError) Error() string {
	if p == nil {
		return "<nil problem>"
	}
	if p.Detail != "" {
		return tokenauth.RedactKnownSecrets(p.Detail)
	}
	return string(p.Code)
}

type problemErrorJSON ProblemError

func (p *ProblemError) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("null"), nil
	}
	safe := *p
	safe.Type = tokenauth.RedactKnownSecrets(safe.Type)
	safe.Title = tokenauth.RedactKnownSecrets(safe.Title)
	safe.Detail = tokenauth.RedactKnownSecrets(safe.Detail)
	safe.Instance = tokenauth.RedactKnownSecrets(safe.Instance)
	safe.Errors = sanitizeProblemErrors(safe.Errors)
	safe.Details = sanitizeProblemDetails(safe.Details)
	return json.Marshal(problemErrorJSON(safe))
}

// GetStatus satisfies huma.StatusError.
func (p *ProblemError) GetStatus() int { return p.Status }

// ContentType rewrites application/json to application/problem+json per
// RFC 9457, mirroring huma's ErrorModel behavior.
func (p *ProblemError) ContentType(ct string) string {
	switch ct {
	case "application/json":
		return "application/problem+json"
	case "application/cbor":
		return "application/problem+cbor"
	default:
		return ct
	}
}

// codeForStatus maps an HTTP status to the default wire code. Used when
// callers reach for huma.Error4xx without specifying a code (e.g. during
// migration) or when our own helpers don't pick a richer code.
func CodeForStatus(status int) ProblemCode {
	switch status {
	case http.StatusBadRequest:
		return CodeBadRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusUnprocessableEntity:
		return CodeValidationError
	case http.StatusTooManyRequests:
		return CodeRateLimited
	case http.StatusServiceUnavailable:
		return CodeServiceUnavailable
	case http.StatusBadGateway:
		return CodeUpstreamError
	default:
		if status >= 500 {
			return CodeInternalError
		}
		return CodeBadRequest
	}
}

// titleForStatus returns the standard HTTP status text for a status code,
// matching the huma.ErrorModel default behavior.
func titleForStatus(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Error"
}

// newProblem is the canonical constructor. Status drives both the wire
// status and (when code is empty) the default code; detail is the
// human-readable message; details is the machine-readable context. The
// returned value satisfies huma.StatusError.
func NewProblem(status int, code ProblemCode, detail string, details map[string]any) *ProblemError {
	if code == "" {
		code = CodeForStatus(status)
	}
	return &ProblemError{
		Status:  status,
		Title:   titleForStatus(status),
		Detail:  tokenauth.RedactKnownSecrets(detail),
		Code:    code,
		Details: sanitizeProblemDetails(details),
	}
}

func sanitizeProblemDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]any, len(details))
	for key, value := range details {
		out[tokenauth.RedactKnownSecrets(key)] = sanitizeProblemValue(value)
	}
	return out
}

func sanitizeProblemValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return tokenauth.RedactKnownSecrets(typed)
	case error:
		return tokenauth.RedactKnownSecrets(typed.Error())
	case fmt.Stringer:
		return tokenauth.RedactKnownSecrets(typed.String())
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = tokenauth.RedactKnownSecrets(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = sanitizeProblemValue(item)
		}
		return out
	case map[string]any:
		return sanitizeProblemDetails(typed)
	default:
		return value
	}
}

func sanitizeProblemErrors(errors []*huma.ErrorDetail) []*huma.ErrorDetail {
	if len(errors) == 0 {
		return nil
	}
	out := make([]*huma.ErrorDetail, 0, len(errors))
	for _, detail := range errors {
		if detail == nil {
			continue
		}
		safe := *detail
		safe.Location = tokenauth.RedactKnownSecrets(safe.Location)
		safe.Message = tokenauth.RedactKnownSecrets(safe.Message)
		safe.Value = sanitizeProblemValue(safe.Value)
		out = append(out, &safe)
	}
	return out
}

// problemBadRequest returns a 400 with the supplied code (defaults to
// CodeBadRequest when "").
func BadRequest(code ProblemCode, detail string, details map[string]any) huma.StatusError {
	if code == "" {
		code = CodeBadRequest
	}
	return NewProblem(http.StatusBadRequest, code, detail, details)
}

// problemValidation returns a 400 with code CodeValidationError, embedding
// the offending field and (optionally) the allowed values. Field is the
// JSON path of the value at fault ("body.status", "query.repo", etc.).
func Validation(field, detail string, allowed ...string) huma.StatusError {
	d := map[string]any{}
	if field != "" {
		d["field"] = field
	}
	if len(allowed) > 0 {
		d["allowed"] = allowed
	}
	if len(d) == 0 {
		d = nil
	}
	return NewProblem(http.StatusBadRequest, CodeValidationError, detail, d)
}

// problemNotFound returns a 404 with the supplied code. Pass CodeNotFound
// for the generic case or one of CodeRepoNotFound, CodePullNotFound, etc.
// for richer semantics.
func NotFound(code ProblemCode, detail string, details map[string]any) huma.StatusError {
	if code == "" {
		code = CodeNotFound
	}
	return NewProblem(http.StatusNotFound, code, detail, details)
}

// problemConflict returns a 409 with the supplied code.
func Conflict(code ProblemCode, detail string, details map[string]any) huma.StatusError {
	if code == "" {
		code = CodeConflict
	}
	return NewProblem(http.StatusConflict, code, detail, details)
}

// problemForbidden returns a 403.
func Forbidden(detail string, details map[string]any) huma.StatusError {
	return NewProblem(http.StatusForbidden, CodeForbidden, detail, details)
}

// GitCredentialUnavailable reports that the executing spoke cannot admit
// networked Git for an otherwise verified repository descriptor.
func GitCredentialUnavailable(provider, host, repoPath string) huma.StatusError {
	details := platformErrorDetails(provider, host)
	if repoPath != "" {
		details["repoPath"] = repoPath
	}
	return NewProblem(
		http.StatusServiceUnavailable,
		CodeGitCredentialUnavailable,
		"Git credentials for this repository are unavailable on this Forge spoke.",
		details,
	)
}

// problemInternal returns a 500.
func Internal(detail string) huma.StatusError {
	return NewProblem(http.StatusInternalServerError, CodeInternalError, detail, nil)
}

// problemUpstream returns a 502 (provider API failure). The optional
// provider/host are surfaced in details when non-empty.
func Upstream(detail, provider, host string) huma.StatusError {
	d := map[string]any{}
	if provider != "" {
		d["provider"] = provider
	}
	if host != "" {
		d["platformHost"] = host
	}
	if len(d) == 0 {
		d = nil
	}
	return NewProblem(http.StatusBadGateway, CodeUpstreamError, detail, d)
}

// MutationOutcomeUnknown returns a 502 for a non-idempotent operation whose
// upstream side effect cannot be safely ruled out. Provider identity is
// included so clients can fence and reconcile the exact affected resource.
func MutationOutcomeUnknown(detail, provider, host string) *ProblemError {
	d := platformErrorDetails(provider, host)
	if len(d) == 0 {
		d = nil
	}
	return NewProblem(http.StatusBadGateway, CodeMutationOutcomeUnknown, detail, d)
}

// problemServiceUnavailable returns a 503.
func ServiceUnavailable(detail string) huma.StatusError {
	return NewProblem(http.StatusServiceUnavailable, CodeServiceUnavailable, detail, nil)
}

// HubUnavailable reports that a spoke cannot reach its federation's
// provider owner. Callers may retry, but must not read local provider tables.
func HubUnavailable(detail string) *ProblemError {
	return NewProblem(
		http.StatusServiceUnavailable,
		CodeHubUnavailable,
		detail,
		map[string]any{"retryable": true},
	)
}

// problemPayloadTooLarge returns a 413 with maxBytes in details when known.
func PayloadTooLarge(detail string, maxBytes int64) huma.StatusError {
	var d map[string]any
	if maxBytes > 0 {
		d = map[string]any{"maxBytes": maxBytes}
	}
	return NewProblem(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, detail, d)
}

// problemUnsupportedCapability returns a 409 with code
// CodeUnsupportedCapability and details {capability, provider,
// platformHost}.
func UnsupportedCapability(repo db.Repo, capability string) huma.StatusError {
	provider := platform.Kind(strings.TrimSpace(repo.Platform))
	if provider == "" {
		provider = platform.KindGitHub
	}
	host := strings.TrimSpace(repo.PlatformHost)
	if host == "" {
		if defaultHost, ok := platform.DefaultHost(provider); ok {
			host = defaultHost
		} else {
			host = platform.DefaultGitHubHost
		}
	}
	details := map[string]any{
		"capability":   capability,
		"provider":     string(provider),
		"platformHost": host,
	}
	return NewProblem(
		http.StatusConflict,
		CodeUnsupportedCapability,
		"Unsupported provider capability",
		details,
	)
}

// problemRateLimited returns a 429 with code CodeRateLimited. retryAfter
// is rendered as an RFC 3339 string when non-nil; provider/host go into
// details when non-empty.
func RateLimited(provider, host string, retryAfter *time.Time) huma.StatusError {
	d := map[string]any{}
	if retryAfter != nil {
		d["retryAfter"] = retryAfter.UTC().Format(time.RFC3339)
	}
	if provider != "" {
		d["provider"] = provider
	}
	if host != "" {
		d["platformHost"] = host
	}
	if len(d) == 0 {
		d = nil
	}
	return NewProblem(
		http.StatusTooManyRequests,
		CodeRateLimited,
		"Upstream rate limit exceeded",
		d,
	)
}

// problemBranchConflict returns the 409 used when a local branch already
// exists with the workspace's requested name. The branch and suggested
// alternative go into details.
func BranchConflict(branch, suggested string) huma.StatusError {
	d := map[string]any{}
	if branch != "" {
		d["branch"] = branch
	}
	if suggested != "" {
		d["suggestedBranch"] = suggested
	}
	if len(d) == 0 {
		d = nil
	}
	return NewProblem(
		http.StatusConflict,
		CodeBranchConflict,
		"A local branch with the requested name already exists.",
		d,
	)
}

// providerCallProblem translates a provider-call error into a wire
// problem. Provider/host narrow the upstream problem when no platform.Error
// is in the chain; when one is, mapPlatformError handles the translation
// (and ignores the provider/host arguments).
func ProviderCallProblem(err error, provider, host string) huma.StatusError {
	return ProviderCallProblemWithDetail(err, provider, host, "")
}

// ProviderMutationProblem preserves provider responses that prove a mutation
// was rejected. Transport, contract, and otherwise unclassified failures are
// ambiguous after dispatch and must fence retries with mutationOutcomeUnknown.
func ProviderMutationProblem(err error, provider, host string) huma.StatusError {
	if err == nil {
		return nil
	}
	if errors.Is(err, tokenauth.ErrMissingToken) {
		return ProviderCallProblem(err, provider, host)
	}
	if pe, ok := errors.AsType[*platform.Error](err); ok {
		switch pe.Code {
		case platform.ErrCodeUnsupportedCapability,
			platform.ErrCodeRepositoryFeatureDisabled,
			platform.ErrCodeProviderNotConfigured,
			platform.ErrCodeMissingToken,
			platform.ErrCodeInvalidRepoRef,
			platform.ErrCodeInvalidArgument,
			platform.ErrCodePermissionDenied,
			platform.ErrCodeNotFound,
			platform.ErrCodeRateLimited,
			platform.ErrCodeStaleState,
			platform.ErrCodeConflict:
			return ProviderCallProblem(err, provider, host)
		}
	}
	return MutationOutcomeUnknown(
		"The provider could not confirm whether the mutation was applied.",
		provider,
		host,
	)
}

func ProviderCallProblemWithDetail(
	err error,
	provider, host, detail string,
) huma.StatusError {
	if err == nil {
		return nil
	}
	if errors.Is(err, tokenauth.ErrMissingToken) {
		if detail == "" {
			detail = err.Error()
		}
		return BadRequest(CodeBadRequest, detail, nil)
	}
	if errors.Is(err, platform.ErrSyncDisabled) {
		return ServiceUnavailable(err.Error())
	}
	if _, ok := errors.AsType[*platform.Error](err); ok {
		if mapped := MapPlatformError(err); mapped != nil {
			return mapped
		}
	}
	if detail == "" {
		detail = err.Error()
	}
	return Upstream(detail, provider, host)
}

// mapPlatformError translates an error from internal/platform into a wire
// problem. Returns nil for nil input or context cancellation so callers
// can propagate those without altering control flow. Returns a generic
// upstream problem with the error's text when no platform.Error is in
// the chain.
func MapPlatformError(err error) huma.StatusError {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	if errors.Is(err, tokenauth.ErrMissingToken) {
		return BadRequest(CodeBadRequest, err.Error(), nil)
	}
	if errors.Is(err, platform.ErrSyncDisabled) {
		return ServiceUnavailable(err.Error())
	}
	var pe *platform.Error
	if !errors.As(err, &pe) {
		return Upstream(err.Error(), "", "")
	}
	provider := string(pe.Provider)
	host := pe.PlatformHost
	switch pe.Code {
	case platform.ErrCodeUnsupportedCapability:
		// We don't have a db.Repo here, so synthesize the details directly
		// rather than going through problemUnsupportedCapability.
		details := map[string]any{
			"capability":   pe.Capability,
			"provider":     provider,
			"platformHost": host,
		}
		return NewProblem(
			http.StatusConflict,
			CodeUnsupportedCapability,
			"Unsupported provider capability",
			details,
		)
	case platform.ErrCodeProviderContract:
		details := platformErrorDetails(provider, host)
		if pe.Field != "" {
			details["field"] = pe.Field
		}
		return NewProblem(
			http.StatusBadGateway,
			CodeUpstreamError,
			err.Error(),
			details,
		)
	case platform.ErrCodeRateLimited:
		return RateLimited(provider, host, pe.ResetAt)
	case platform.ErrCodePermissionDenied:
		d := platformErrorDetails(provider, host)
		if len(d) == 0 {
			d = nil
		}
		return Forbidden(err.Error(), d)
	case platform.ErrCodeNotFound:
		// A moved-lookup not_found carries the destination repository;
		// surface the full provider-aware identity as stable extension
		// members so clients can retarget the reference instead of
		// parsing prose.
		var details map[string]any
		if pe.Destination != nil {
			details = platformErrorDetails(provider, host)
			details["destinationProvider"] = string(pe.Destination.Platform)
			details["destinationPlatformHost"] = pe.Destination.Host
			details["destinationOwner"] = pe.Destination.Owner
			details["destinationName"] = pe.Destination.Name
		}
		return NotFound(CodeNotFound, err.Error(), details)
	// Both conflict flavors share wire code `conflict` and HTTP 409;
	// details.reason is the stable discriminator clients branch on
	// (stale_state: reload and re-review; conflict: the provider refuses
	// the current state, re-reviewing won't help by itself).
	case platform.ErrCodeStaleState:
		d := platformErrorDetails(provider, host)
		d["reason"] = "stale_state"
		detail := "target changed since it was reviewed; refresh and retry"
		// Surface provider side-effect context — an approval that
		// could not be revoked, a review note already posted — instead
		// of hiding it behind the generic re-review prompt. Stable
		// side-effect members (revocation, review_id) merge alongside
		// so clients can branch without parsing prose.
		if pe.Hint != "" {
			d["context"] = pe.Hint
			detail += "; " + pe.Hint
		}
		for key, value := range pe.Details {
			if _, reserved := d[key]; !reserved {
				d[key] = value
			}
		}
		return Conflict(CodeConflict, detail, d)
	case platform.ErrCodeConflict:
		d := platformErrorDetails(provider, host)
		d["reason"] = "conflict"
		for key, value := range pe.Details {
			if key == "reason" {
				d[key] = value
				continue
			}
			if _, reserved := d[key]; !reserved {
				d[key] = value
			}
		}
		return Conflict(CodeConflict, err.Error(), d)
	case platform.ErrCodeProviderNotConfigured,
		platform.ErrCodeMissingToken,
		platform.ErrCodeInvalidRepoRef,
		platform.ErrCodeInvalidArgument:
		return BadRequest(CodeBadRequest, err.Error(), nil)
	default:
		return Upstream(err.Error(), provider, host)
	}
}

// platformErrorDetails seeds a problem details map with provider
// identity; callers add their extension members on top.
func platformErrorDetails(provider, host string) map[string]any {
	d := map[string]any{}
	if provider != "" {
		d["provider"] = provider
	}
	if host != "" {
		d["platformHost"] = host
	}
	return d
}

// init replaces huma.NewError so any remaining huma.ErrorNxx callers
// (or huma's own internal validators) emit a ProblemError envelope.
// Without this hook, migration would be all-or-nothing: any missed call
// site would emit a body with no code field. With it, every code path
// already produces a code (status-derived) and migration becomes
// incremental.
func init() {
	huma.DefaultArrayNullable = false
	huma.DefaultJSONFormat = huma.Format{
		Marshal: func(w io.Writer, value any) error {
			return jsonv2.MarshalWrite(w, value)
		},
		Unmarshal: func(data []byte, value any) error {
			return jsonv2.Unmarshal(data, value)
		},
	}
	huma.DefaultFormats["application/json"] = huma.DefaultJSONFormat
	huma.DefaultFormats["json"] = huma.DefaultJSONFormat

	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		details := make([]*huma.ErrorDetail, 0, len(errs))
		for _, e := range errs {
			if e == nil {
				continue
			}
			if d, ok := e.(huma.ErrorDetailer); ok {
				details = append(details, d.ErrorDetail())
				continue
			}
			details = append(details, &huma.ErrorDetail{Message: e.Error()})
		}
		p := NewProblem(status, CodeForStatus(status), msg, nil)
		if len(details) > 0 {
			p.Errors = sanitizeProblemErrors(details)
		}
		return p
	}
}
