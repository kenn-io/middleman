package platform

import (
	"errors"
	"fmt"
	"time"
)

type PlatformErrorCode string

const (
	RepositoryFeatureIssues        = "issues"
	RepositoryFeatureMergeRequests = "merge_requests"
)

const (
	ErrCodeUnsupportedCapability     PlatformErrorCode = "unsupported_capability"
	ErrCodeRepositoryFeatureDisabled PlatformErrorCode = "repository_feature_disabled"
	ErrCodeProviderContract          PlatformErrorCode = "provider_contract"
	ErrCodeProviderNotConfigured     PlatformErrorCode = "provider_not_configured"
	ErrCodeMissingToken              PlatformErrorCode = "missing_token"
	ErrCodeInvalidRepoRef            PlatformErrorCode = "invalid_repo_ref"
	ErrCodeInvalidArgument           PlatformErrorCode = "invalid_argument"
	ErrCodePermissionDenied          PlatformErrorCode = "permission_denied"
	ErrCodeNotFound                  PlatformErrorCode = "not_found"
	ErrCodeRateLimited               PlatformErrorCode = "rate_limited"
	// ErrCodeStaleState marks mutations rejected because the target moved
	// past the state the caller acted on (for example an MR head SHA that
	// advanced after review).
	ErrCodeStaleState PlatformErrorCode = "stale_state"
	// ErrCodeConflict marks provider conflicts that are not staleness:
	// the request was understood but the target's current state refuses
	// it (for example merging an unmergeable MR).
	ErrCodeConflict PlatformErrorCode = "conflict"
	// ErrCodePageLimit marks a whole-dataset drain that exceeded the
	// caller-side page budget. The provider did nothing wrong — the dataset
	// is larger than a single in-memory collection supports; unbounded
	// datasets belong on the durable-cursor archive path.
	ErrCodePageLimit PlatformErrorCode = "page_limit"
)

var (
	ErrUnsupportedCapability     = &Error{Code: ErrCodeUnsupportedCapability}
	ErrRepositoryFeatureDisabled = &Error{Code: ErrCodeRepositoryFeatureDisabled}
	ErrProviderContract          = &Error{Code: ErrCodeProviderContract}
	ErrProviderNotConfigured     = &Error{Code: ErrCodeProviderNotConfigured}
	ErrMissingToken              = &Error{Code: ErrCodeMissingToken}
	ErrInvalidRepoRef            = &Error{Code: ErrCodeInvalidRepoRef}
	ErrInvalidArgument           = &Error{Code: ErrCodeInvalidArgument}
	ErrPermissionDenied          = &Error{Code: ErrCodePermissionDenied}
	ErrNotFound                  = &Error{Code: ErrCodeNotFound}
	ErrRateLimited               = &Error{Code: ErrCodeRateLimited}
	ErrStaleState                = &Error{Code: ErrCodeStaleState}
	ErrPageLimit                 = &Error{Code: ErrCodePageLimit}
)

// ErrArchiveAttemptBudget is returned by budget-counting transports when an
// archive request exhausts its admitted wire-attempt allowance. Archive work
// treats it as a transient budget deferral and must never let it surface as a
// repository-blocking contract error.
var ErrArchiveAttemptBudget = errors.New("archive wire-attempt allowance exhausted")

// ErrSyncBudgetExhausted is returned before provider I/O when the process-local
// emergency ceiling cannot reserve another background wire attempt.
var ErrSyncBudgetExhausted = errors.New("local sync emergency ceiling exhausted")

// ErrSyncDisabled is returned before provider I/O when synchronization is
// disabled for this process.
var ErrSyncDisabled = errors.New("provider sync is disabled")

// ErrLookupInaccessible marks a single-item lookup that the provider has
// explicitly classified as inaccessible rather than a generic authentication
// or permission failure.
var ErrLookupInaccessible = errors.New("lookup explicitly classified as inaccessible")

// ErrLookupNotPresent marks a parent item lookup that the provider has
// explicitly classified as removed or moved.
var ErrLookupNotPresent = errors.New("parent item lookup explicitly classified as not present")

type Error struct {
	Code         PlatformErrorCode
	Provider     Kind
	PlatformHost string
	Capability   string
	TokenEnv     string
	Field        string
	ResetAt      *time.Time
	// Hint carries client-safe side-effect context that must survive
	// problem mapping — e.g. an approval that could not be revoked
	// after the head moved, or a review note that was already posted
	// before the failure. Err keeps the full chain for logs; Hint is
	// what an API client needs to act on the side effect.
	Hint string
	// Details carries stable, client-safe extension members merged
	// into the problem details map (e.g. revocation: "failed",
	// review_id: "31") so clients can branch on side-effect outcomes
	// without parsing Hint prose. Keys must not collide with the
	// reserved problem members (reason, provider, platformHost).
	Details map[string]string
	// Destination identifies where an item now lives when a not_found error
	// stems from a moved lookup outcome (repository transfer). Callers still
	// branch on Code; the destination is structured, actionable detail for
	// retargeting the reference instead of parsing prose.
	Destination *RepoRef
	Err         error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}

	message := string(e.Code)
	if e.Provider != "" || e.PlatformHost != "" {
		message = fmt.Sprintf("%s for %s/%s", message, e.Provider, e.PlatformHost)
	}
	if e.Capability != "" {
		message = fmt.Sprintf("%s: %s", message, e.Capability)
	}
	if e.Err != nil {
		message = fmt.Sprintf("%s: %v", message, e.Err)
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	var targetErr *Error
	if !errors.As(target, &targetErr) {
		return false
	}
	return e != nil && e.Code == targetErr.Code
}

func ProviderNotConfigured(kind Kind, host string) error {
	return &Error{
		Code:         ErrCodeProviderNotConfigured,
		Provider:     kind,
		PlatformHost: host,
	}
}

func UnsupportedCapability(kind Kind, host, capability string) error {
	return &Error{
		Code:         ErrCodeUnsupportedCapability,
		Provider:     kind,
		PlatformHost: host,
		Capability:   capability,
	}
}

func RepositoryFeatureDisabled(kind Kind, host, capability string, err error) error {
	return &Error{
		Code:         ErrCodeRepositoryFeatureDisabled,
		Provider:     kind,
		PlatformHost: host,
		Capability:   capability,
		Err:          err,
	}
}

func PermissionDenied(kind Kind, host string, err error) error {
	return &Error{
		Code: ErrCodePermissionDenied, Provider: kind, PlatformHost: host, Err: err,
	}
}

func ProviderContract(kind Kind, host, field string, err error) error {
	// Provider response validation may itself use typed caller-side helpers.
	// Strip those classifications here: an upstream contract failure must not
	// also match invalid_repo_ref or another request error through Unwrap.
	if err != nil {
		err = errors.New(err.Error())
	}
	return &Error{
		Code:         ErrCodeProviderContract,
		Provider:     kind,
		PlatformHost: host,
		Field:        field,
		Err:          err,
	}
}
