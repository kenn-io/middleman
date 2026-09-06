package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/platform"
)

// ListIssuesPage owns GitHub issue inventory requests and normalization.
func (p *Provider) ListIssuesPage(
	ctx context.Context,
	ref platform.RepoRef,
	query platform.ItemPageQuery,
) (platform.Page[platform.Issue], error) {
	if err := platform.ValidateItemPageQuery(query); err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	if query.Order == platform.ItemOrderUpdated {
		since := time.Time{}
		if query.UpdatedSince != nil {
			since = query.UpdatedSince.UTC()
		}
		return p.listInventoryIssuesPage(ctx, ref, query.Cursor, "updated_issues", "updated", since)
	}
	return p.listInventoryIssuesPage(ctx, ref, query.Cursor, "historical_issues", "created", time.Time{})
}

// ListMergeRequestsPage owns GitHub merge-request inventory requests and normalization.
func (p *Provider) ListMergeRequestsPage(
	ctx context.Context,
	ref platform.RepoRef,
	query platform.ItemPageQuery,
) (platform.Page[platform.MergeRequest], error) {
	if err := platform.ValidateItemPageQuery(query); err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	if query.Order == platform.ItemOrderUpdated {
		since := time.Time{}
		if query.UpdatedSince != nil {
			since = query.UpdatedSince.UTC()
		}
		return p.listInventoryMergeRequestsPage(
			ctx, ref, query.Cursor, "updated_merge_requests", "updated", since,
		)
	}
	return p.listInventoryMergeRequestsPage(
		ctx, ref, query.Cursor, "historical_merge_requests", "created", time.Time{},
	)
}

type lookupOutcome string

const (
	lookupRemoved      lookupOutcome = "removed"
	lookupMoved        lookupOutcome = "moved"
	lookupInaccessible lookupOutcome = "inaccessible"
)

func RepositoryFeatureDisabled(host, capability string, err error) error {
	var responseErr *gh.ErrorResponse
	if !errors.As(err, &responseErr) || responseErr.Response == nil ||
		responseErr.Response.StatusCode != http.StatusGone {
		return nil
	}

	message := strings.ToLower(responseErr.Message)
	var phrase string
	switch capability {
	case platform.RepositoryFeatureIssues:
		phrase = "issues are disabled"
	case platform.RepositoryFeatureMergeRequests:
		phrase = "pull requests are disabled"
	default:
		return nil
	}
	if !strings.Contains(message, phrase) {
		return nil
	}
	return platform.RepositoryFeatureDisabled(platform.KindGitHub, host, capability, err)
}

func (p *Provider) classifyIssueLookup(
	ctx context.Context,
	ref platform.RepoRef,
	err error,
) (lookupOutcome, *platform.RepoRef, error) {
	if disabledErr := RepositoryFeatureDisabled(
		p.host, platform.RepositoryFeatureIssues, err,
	); disabledErr != nil {
		return "", nil, disabledErr
	}
	// GitHub documents 410 from the single-issue endpoint as a deleted
	// issue. Repository-wide issue disablement uses the same status but is
	// classified above, so every remaining 410 here is a definitive parent
	// removal rather than a transient transport failure. Keep this mapping at
	// the issue lookup boundary; 410 has different meanings on other endpoints.
	if StatusCode(err) == http.StatusGone {
		return lookupRemoved, nil, nil
	}
	mapped := p.archiveTransportError(platform.ArchiveCapabilityHistoricalIssues, err)
	if errors.Is(mapped, platform.ErrRateLimited) {
		return "", nil, mapped
	}
	status := StatusCode(err)
	if status != http.StatusNotFound {
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			if _, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name); repoErr != nil {
				return "", nil, p.archiveRepositoryProbeError(repoErr)
			}
			return lookupInaccessible, nil, nil
		}
		return "", nil, mapped
	}
	if _, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name); repoErr != nil {
		return "", nil, p.archiveRepositoryProbeError(repoErr)
	}
	return lookupRemoved, nil, nil
}

func (p *Provider) classifyMergeRequestLookup(
	ctx context.Context,
	ref platform.RepoRef,
	err error,
) (lookupOutcome, *platform.RepoRef, error) {
	if disabledErr := RepositoryFeatureDisabled(
		p.host, platform.RepositoryFeatureMergeRequests, err,
	); disabledErr != nil {
		return "", nil, disabledErr
	}
	mapped := p.archiveTransportError(platform.ArchiveCapabilityHistoricalMergeRequests, err)
	if errors.Is(mapped, platform.ErrRateLimited) {
		return "", nil, mapped
	}
	status := StatusCode(err)
	if status != http.StatusNotFound {
		if status == http.StatusForbidden || status == http.StatusUnauthorized {
			if _, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name); repoErr != nil {
				return "", nil, p.archiveRepositoryProbeError(repoErr)
			}
			return lookupInaccessible, nil, nil
		}
		return "", nil, mapped
	}
	if _, repoErr := p.client.GetRepository(ctx, ref.Owner, ref.Name); repoErr != nil {
		return "", nil, p.archiveRepositoryProbeError(repoErr)
	}
	return lookupRemoved, nil, nil
}

// IssueLookupOutcomeError maps a raw single-issue fetch result onto the
// canonical lookup classification so the optimized GitHub detail path
// surfaces the same typed outcomes as LookupIssue: removed is not_found,
// inaccessible is permission_denied, and a repository transfer is not_found
// carrying the destination. A nil return means the result needs no outcome
// mapping. Classification may spend one repository probe on the live client;
// this runs in live sync, not archive admission, so no admitted budget
// applies.
func (p *Provider) IssueLookupOutcomeError(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	issue *gh.Issue,
	err error,
) error {
	if err != nil {
		outcome, destination, classifyErr := p.classifyIssueLookup(ctx, ref, err)
		if classifyErr != nil {
			return classifyErr
		}
		return p.lookupNotPresentError(ref, number, outcome, destination)
	}
	if issue == nil {
		return nil
	}
	if destination := ArchiveDestination(ref, issue.GetRepositoryURL()); destination != nil {
		return p.lookupNotPresentError(ref, number, lookupMoved, destination)
	}
	return nil
}

// IssuePullRequestOutcomeError classifies an issue fetch whose number
// resolved to a pull request. REST serves pull requests from the issues
// endpoint, but an issue number that resolves to a pull request is not an
// issue: surfacing it as present hands downstream issue reads (GraphQL
// timeline, comments) a number they can never resolve, so hydration retries
// forever. Callers that dispatch on the fetched item's kind (issue vs pull
// request) must not apply this; it is for reads that require an issue.
func (p *Provider) IssuePullRequestOutcomeError(
	ref platform.RepoRef,
	number int,
	issue *gh.Issue,
) error {
	if issue == nil || !issue.IsPullRequest() {
		return nil
	}
	return p.lookupNotPresentError(ref, number, lookupRemoved, nil)
}

// MergeRequestLookupOutcomeError is the merge-request counterpart to
// IssueLookupOutcomeError.
func (p *Provider) MergeRequestLookupOutcomeError(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	pr *gh.PullRequest,
	err error,
) error {
	if err != nil {
		outcome, destination, classifyErr := p.classifyMergeRequestLookup(ctx, ref, err)
		if classifyErr != nil {
			return classifyErr
		}
		return p.lookupNotPresentError(ref, number, outcome, destination)
	}
	if pr == nil {
		return nil
	}
	if destination := ArchiveDestination(ref, pr.GetBase().GetRepo().GetURL()); destination != nil {
		return p.lookupNotPresentError(ref, number, lookupMoved, destination)
	}
	return nil
}
func (p *Provider) listInventoryIssuesPage(
	ctx context.Context,
	ref platform.RepoRef,
	cursor string,
	mode string,
	sortBy string,
	since time.Time,
) (platform.Page[platform.Issue], error) {
	client, err := p.InventoryAPI()
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	state, err := decodeGitHubArchiveCursor(cursor, ref, mode, since)
	if err != nil {
		return platform.Page[platform.Issue]{}, platform.ProviderContract(
			platform.KindGitHub, p.host, "archive_cursor", err,
		)
	}
	querySince, err := githubArchiveIssueSince(mode, state.Since)
	if err != nil {
		return platform.Page[platform.Issue]{}, platform.ProviderContract(
			platform.KindGitHub, p.host, "archive_cursor", err,
		)
	}
	items, next, exhausted, err := client.ListInventoryIssuesPage(
		ctx, ref.Owner, ref.Name, sortBy, state.After, querySince,
	)
	if err != nil {
		return platform.Page[platform.Issue]{}, p.archiveTransportError(platform.ArchiveCapabilityHistoricalIssues, err)
	}
	out := make([]platform.Issue, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		normalized, err := NormalizeIssue(ref, item)
		if err != nil {
			return platform.Page[platform.Issue]{}, err
		}
		out = append(out, normalized)
	}
	if exhausted {
		return platform.Page[platform.Issue]{Items: out, Exhausted: true}, nil
	}
	state.After = next
	encoded, err := encodeGitHubArchiveCursor(state)
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	return platform.Page[platform.Issue]{Items: out, NextCursor: encoded}, nil
}

// githubArchiveIssueSince overlaps the maintenance issue scan by one second so
// the inclusive watermark contract is honored against GitHub's exclusive
// GraphQL since filter.
func githubArchiveIssueSince(mode, since string) (string, error) {
	if mode != "updated_issues" || since == "" {
		return since, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		return "", fmt.Errorf("parse issue maintenance watermark: %w", err)
	}
	return parsed.Add(-time.Second).Format(time.RFC3339Nano), nil
}

// listInventoryMergeRequestsPage owns the REST historical and maintenance
// merge-request request shapes. The historical scan traverses ascending by
// creation time; the maintenance scan traverses descending by update time and
// stops once it crosses the overlapped watermark.
func (p *Provider) listInventoryMergeRequestsPage(
	ctx context.Context,
	ref platform.RepoRef,
	cursor string,
	mode string,
	sortBy string,
	since time.Time,
) (platform.Page[platform.MergeRequest], error) {
	client, err := p.InventoryAPI()
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	state, err := decodeGitHubArchiveCursor(cursor, ref, mode, since)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, platform.ProviderContract(
			platform.KindGitHub, p.host, "archive_cursor", err,
		)
	}
	items, hasMore, err := client.ListInventoryPullRequestsPage(
		ctx, ref.Owner, ref.Name, sortBy, state.Page,
	)
	if err != nil {
		if disabledErr := p.mergeRequestsDisabledByRepository(ctx, ref, err); disabledErr != nil {
			return platform.Page[platform.MergeRequest]{}, disabledErr
		}
		return platform.Page[platform.MergeRequest]{}, p.archiveTransportError(platform.ArchiveCapabilityHistoricalMergeRequests, err)
	}
	out := make([]platform.MergeRequest, 0, len(items))
	crossedWatermark := false
	overlapStart := since.Add(-time.Second)
	for _, item := range items {
		normalized, err := NormalizePullRequest(ref, item)
		if err != nil {
			return platform.Page[platform.MergeRequest]{}, err
		}
		if mode == "updated_merge_requests" && normalized.UpdatedAt.Before(overlapStart) {
			crossedWatermark = true
			continue
		}
		out = append(out, normalized)
	}
	if crossedWatermark {
		return platform.Page[platform.MergeRequest]{Items: out, Exhausted: true}, nil
	}
	return pageWithNext(out, state, hasMore)
}

var (
	_ platform.IssuePageReader        = (*Provider)(nil)
	_ platform.MergeRequestPageReader = (*Provider)(nil)
)

type InventoryAPI interface {
	ListInventoryIssuesPage(
		context.Context, string, string, string, string, string,
	) ([]*gh.Issue, string, bool, error)
	ListInventoryPullRequestsPage(
		context.Context, string, string, string, int,
	) ([]*gh.PullRequest, bool, error)
}

type githubArchiveCursor struct {
	Mode  string `json:"mode"`
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Page  int    `json:"page"`
	After string `json:"after,omitempty"`
	Since string `json:"since,omitempty"`
}

func (p *Provider) archiveRepositoryProbeError(err error) error {
	mapped := p.archiveTransportError("", err)
	if errors.Is(mapped, platform.ErrRateLimited) {
		return mapped
	}
	switch StatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return platform.PermissionDenied(platform.KindGitHub, p.host, err)
	default:
		return mapped
	}
}

func (p *Provider) archiveTransportError(capability platform.ArchiveCapability, err error) error {
	return mapGitHubReadError(p.host, p.now, capability, err)
}

func mapGitHubReadError(host string, now func() time.Time, capability platform.ArchiveCapability, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch capability {
	case platform.ArchiveCapabilityHistoricalIssues:
		if disabled := RepositoryFeatureDisabled(
			host, platform.RepositoryFeatureIssues, err,
		); disabled != nil {
			return disabled
		}
	case platform.ArchiveCapabilityHistoricalMergeRequests:
		if disabled := RepositoryFeatureDisabled(
			host, platform.RepositoryFeatureMergeRequests, err,
		); disabled != nil {
			return disabled
		}
	}
	if existing, ok := errors.AsType[*platform.Error](err); ok {
		mapped := *existing
		if mapped.Provider == "" {
			mapped.Provider = platform.KindGitHub
		}
		if mapped.PlatformHost == "" {
			mapped.PlatformHost = host
		}
		if mapped.Capability == "" {
			mapped.Capability = string(capability)
		}
		return &mapped
	}
	response := githubArchiveErrorResponse(err)
	resetAt := ArchiveResetAt(response)
	if rateLimit, ok := errors.AsType[*gh.RateLimitError](err); ok {
		if !rateLimit.Rate.Reset.IsZero() {
			reset := rateLimit.Rate.Reset.UTC()
			resetAt = &reset
		}
		return &platform.Error{Code: platform.ErrCodeRateLimited, Provider: platform.KindGitHub,
			PlatformHost: host, Capability: string(capability), ResetAt: resetAt, Err: err}
	}
	if abuseLimit, ok := errors.AsType[*gh.AbuseRateLimitError](err); ok {
		if resetAt == nil && abuseLimit.RetryAfter != nil {
			reset := now().UTC().Add(*abuseLimit.RetryAfter)
			resetAt = &reset
		}
		return &platform.Error{Code: platform.ErrCodeRateLimited, Provider: platform.KindGitHub,
			PlatformHost: host, Capability: string(capability), ResetAt: resetAt, Err: err}
	}
	status := StatusCode(err)
	if status == http.StatusTooManyRequests ||
		status == http.StatusForbidden && response != nil && response.Header.Get("X-RateLimit-Remaining") == "0" {
		return &platform.Error{Code: platform.ErrCodeRateLimited, Provider: platform.KindGitHub,
			PlatformHost: host, Capability: string(capability), ResetAt: resetAt, Err: err}
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		return &platform.Error{Code: platform.ErrCodePermissionDenied, Provider: platform.KindGitHub,
			PlatformHost: host, Capability: string(capability), Err: err}
	}
	return err
}

func githubArchiveErrorResponse(err error) *http.Response {
	if rateLimit, ok := errors.AsType[*gh.RateLimitError](err); ok {
		return rateLimit.Response
	}
	if abuseLimit, ok := errors.AsType[*gh.AbuseRateLimitError](err); ok {
		return abuseLimit.Response
	}
	if response, ok := errors.AsType[*gh.ErrorResponse](err); ok {
		return response.Response
	}
	return nil
}
func (p *Provider) InventoryAPI() (InventoryAPI, error) {
	client, ok := p.client.(InventoryAPI)
	if !ok {
		return nil, platform.UnsupportedCapability(
			platform.KindGitHub, p.host, string(platform.ArchiveCapabilityHistoricalIssues),
		)
	}
	return client, nil
}

func pageWithNext[T any](
	items []T,
	cursor githubArchiveCursor,
	hasMore bool,
) (platform.Page[T], error) {
	if !hasMore {
		return platform.Page[T]{Items: items, Exhausted: true}, nil
	}
	cursor.Page++
	next, err := encodeGitHubArchiveCursor(cursor)
	if err != nil {
		return platform.Page[T]{}, err
	}
	return platform.Page[T]{Items: items, NextCursor: next}, nil
}

func decodeGitHubArchiveCursor(
	encoded string,
	ref platform.RepoRef,
	mode string,
	since time.Time,
) (githubArchiveCursor, error) {
	expectedSince := ""
	if !since.IsZero() {
		expectedSince = since.UTC().Format(time.RFC3339Nano)
	}
	if encoded == "" {
		return githubArchiveCursor{
			Mode: mode, Host: ref.Host, Owner: ref.Owner, Repo: ref.Name,
			Page: 1, Since: expectedSince,
		}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return githubArchiveCursor{}, fmt.Errorf("decode cursor: %w", err)
	}
	var cursor githubArchiveCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return githubArchiveCursor{}, fmt.Errorf("parse cursor: %w", err)
	}
	if cursor.Mode != mode || cursor.Host != ref.Host || cursor.Owner != ref.Owner || cursor.Repo != ref.Name ||
		cursor.Since != expectedSince || cursor.Page <= 0 {
		return githubArchiveCursor{}, errors.New("cursor does not match archive enumeration")
	}
	return cursor, nil
}

func encodeGitHubArchiveCursor(cursor githubArchiveCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode archive cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func ArchiveDestination(ref platform.RepoRef, repositoryURL string) *platform.RepoRef {
	parsed, err := url.Parse(repositoryURL)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := range parts {
		if parts[i] != "repos" || i+2 >= len(parts) {
			continue
		}
		destination := ref
		destination.Owner = strings.ToLower(parts[i+1])
		destination.Name = strings.ToLower(parts[i+2])
		destination.RepoPath = destination.Owner + "/" + destination.Name
		destination.PlatformID = 0
		destination.PlatformExternalID = ""
		destination.WebURL = ""
		destination.CloneURL = ""
		destination.DefaultBranch = ""
		// GitHub owner/repo names are case-insensitive (canonical
		// kenn-forge identity lowercases them per the platform metadata's
		// LowercaseRepoNames), so a source ref that differs from the
		// returned repository URL only in casing is the same repository,
		// not a transfer.
		if strings.EqualFold(destination.Owner, ref.Owner) &&
			strings.EqualFold(destination.Name, ref.Name) {
			return nil
		}
		return &destination
	}
	return nil
}

func StatusCode(err error) int {
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return response.Response.StatusCode
	}
	if redirect, ok := errors.AsType[*url.Error](err); ok {
		var responseError *gh.ErrorResponse
		if errors.As(redirect.Err, &responseError) && responseError.Response != nil {
			return responseError.Response.StatusCode
		}
	}
	return 0
}

var _ InventoryAPI = (*Client)(nil)
