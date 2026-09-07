package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"go.kenn.io/forge/platform"
)

// gitLabPageCursor binds an opaque resumable cursor to the enumeration that
// produced it: the query mode, the repository identity, the item number for
// detail datasets, and the maintenance watermark. Reusing a cursor with a
// different shape is a provider-contract violation. Exactly one continuation
// form is populated per traversal kind: Page for offset traversals and Link
// for keyset traversals.
type gitLabPageCursor struct {
	Mode     string `json:"mode"`
	Host     string `json:"host"`
	RepoPath string `json:"repo_path"`
	Number   int    `json:"number,omitempty"`
	Page     int64  `json:"page,omitempty"`
	Since    string `json:"since,omitempty"`
	// KeysetCursor is the provider-issued keyset continuation token (the
	// cursor query parameter of the response's next link). Only this one
	// token is ever replayed on resumption; order, state, page size, and
	// watermark are rebuilt from the validated cursor fields, so a tampered
	// cursor cannot override the query shape.
	KeysetCursor string `json:"keyset,omitempty"`
}

// gitLabCursorShape names the continuation form a traversal mode expects, so a
// decoded cursor cannot smuggle another mode's continuation state.
type gitLabCursorShape int

const (
	gitLabCursorOffset gitLabCursorShape = iota
	gitLabCursorKeyset
)

// ListIssuesPage is the single owner of GitLab issue inventory requests and
// their normalization. It dispatches on the query: StateOpen drains the open
// list into one exhausted page, StateAll ordered-by-created pages ascending
// with a resumable cursor, and StateAll ordered-by-updated pages descending
// behind an inclusive watermark served through GitLab's exclusive
// updated_after filter.
func (c *Client) ListIssuesPage(
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
		return c.listInventoryIssuesPage(ctx, ref, since, query.Cursor, "updated_issues", "updated_at")
	}
	return c.listInventoryIssuesPage(ctx, ref, time.Time{}, query.Cursor, "historical_issues", "created_at")
}

// ListMergeRequestsPage is the merge-request counterpart to ListIssuesPage and
// keeps the open-scan merge-status recheck flag inside the canonical open
// branch.
func (c *Client) ListMergeRequestsPage(
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
		return c.listInventoryMergeRequestsPage(
			ctx, ref, since, query.Cursor, "updated_merge_requests", "updated_at",
		)
	}
	return platform.Page[platform.MergeRequest]{}, platform.UnsupportedCapability(
		platform.KindGitLab, c.host, string(platform.ArchiveCapabilityHistoricalMergeRequests),
	)
}

// listInventoryIssuesPage owns the historical and maintenance issue request
// shapes. Both traversals use GitLab's keyset pagination (supported for
// project issues with order_by created_at/updated_at since GitLab 18.3), so
// the server's cursor carries its own id tie-break and equal timestamps have
// a total order across page boundaries. Ordering deviation from the neutral
// contract: created-order ties break by provider record id (the keyset
// tie-break column), not by item number; both are monotone with insertion, so
// restart stability holds. The maintenance traversal runs ascending — under a
// keyset cursor an item whose updated_at moves mid-scan only moves forward
// past the cursor, so it is re-served rather than skipped. Servers without
// issue keyset support (GitLab before 18.3) ignore the pagination parameter
// and answer with offset pagination; that response shape is detected and
// rejected with a typed unsupported_capability error instead of silently
// degrading to a skippable offset traversal.
func (c *Client) listInventoryIssuesPage(
	ctx context.Context,
	ref platform.RepoRef,
	since time.Time,
	encodedCursor string,
	mode string,
	orderBy string,
) (platform.Page[platform.Issue], error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	cursor, err := c.decodePageCursor(normalizedRef, 0, encodedCursor, mode, since, gitLabCursorKeyset)
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	state := "all"
	opts := &gitlab.ListProjectIssuesOptions{
		State: &state,
		ListOptions: gitlab.ListOptions{
			Pagination: "keyset", OrderBy: orderBy, Sort: "asc", PerPage: defaultPageSize,
		},
	}
	if !since.IsZero() {
		overlap := inclusiveGitLabWatermark(since)
		opts.UpdatedAfter = &overlap
	}
	requestOptions := []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)}
	if cursor.KeysetCursor != "" {
		// Replay only the continuation token: the link a provider (or a
		// tampered cursor) issued never contributes any other parameter.
		requestOptions = append(requestOptions, gitlab.WithKeysetPaginationParameters(
			"?"+url.Values{"cursor": {cursor.KeysetCursor}}.Encode(),
		))
	}
	issues, resp, err := c.api.Issues.ListProjectIssues(pid, opts, requestOptions...)
	if err != nil {
		return platform.Page[platform.Issue]{}, c.repositoryFeatureError(
			ctx, normalizedRef, platform.RepositoryFeatureIssues,
			string(platform.ArchiveCapabilityHistoricalIssues), err,
		)
	}
	token, err := c.keysetContinuationToken(resp)
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	items := make([]platform.Issue, 0, len(issues))
	for _, issue := range issues {
		items = append(items, NormalizeIssue(normalizedRef, issue))
	}
	return gitLabKeysetPage(items, cursor, token)
}

// listInventoryMergeRequestsPage owns the maintenance merge-request request
// shape. GitLab project merge requests do not support keyset pagination, so
// maintenance uses bounded offset-descending pages behind an inclusive
// updated_after watermark. Historical inventory is not advertised because
// offset ordering cannot safely enumerate equal-created_at ties.
func (c *Client) listInventoryMergeRequestsPage(
	ctx context.Context,
	ref platform.RepoRef,
	since time.Time,
	encodedCursor string,
	mode string,
	orderBy string,
) (platform.Page[platform.MergeRequest], error) {
	pid, normalizedRef, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	cursor, err := c.decodePageCursor(
		normalizedRef, 0, encodedCursor, mode, since, gitLabCursorOffset,
	)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	state, sortOrder := "all", "desc"
	overlap := inclusiveGitLabWatermark(since)
	mrs, resp, err := c.api.MergeRequests.ListProjectMergeRequests(
		pid,
		&gitlab.ListProjectMergeRequestsOptions{
			State: &state, OrderBy: &orderBy, Sort: &sortOrder, UpdatedAfter: &overlap,
			Page: cursor.Page, PerPage: defaultPageSize,
		},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, c.repositoryFeatureError(
			ctx, normalizedRef, platform.RepositoryFeatureMergeRequests,
			"read_merge_requests", err,
		)
	}
	items := make([]platform.MergeRequest, 0, len(mrs))
	for _, mr := range mrs {
		item := NormalizeMergeRequest(normalizedRef, mr, nil)
		item.HeadRepoCloneURLUnknown = true
		items = append(items, item)
	}
	nextPage := nextGitLabPage(resp)
	if nextPage == 0 {
		return platform.Page[platform.MergeRequest]{Items: items, Exhausted: true}, nil
	}
	cursor.Page = nextPage
	next, err := encodeGitLabPageCursor(cursor)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	return platform.Page[platform.MergeRequest]{Items: items, NextCursor: next}, nil
}

func inclusiveGitLabWatermark(since time.Time) time.Time {
	return since.UTC().Add(-time.Nanosecond)
}

// keysetContinuationToken extracts the continuation token from a keyset
// response and detects servers that ignored the keyset request: GitLab
// releases before 18.3 do not support keyset pagination for project issues
// and answer offset-shaped (an X-Next-Page header and a page-numbered next
// link without a cursor token). Silently following that shape would degrade
// the traversal to offset pagination without its no-skip guarantees, so any
// offset-shaped continuation is rejected with a typed unsupported_capability
// error. A response needing no continuation is accepted regardless of shape:
// a complete single page is identical under either pagination mode.
func (c *Client) keysetContinuationToken(resp *gitlab.Response) (string, error) {
	if resp == nil {
		return "", nil
	}
	if resp.NextPage > 0 {
		return "", c.keysetUnsupportedError()
	}
	if resp.NextLink == "" {
		return "", nil
	}
	nextURL, err := url.Parse(resp.NextLink)
	if err != nil {
		return "", platform.ProviderContract(platform.KindGitLab, c.host, "keyset_next_link", err)
	}
	token := nextURL.Query().Get("cursor")
	if token == "" {
		return "", c.keysetUnsupportedError()
	}
	return token, nil
}

func (c *Client) keysetUnsupportedError() error {
	return &platform.Error{
		Code:         platform.ErrCodeUnsupportedCapability,
		Provider:     platform.KindGitLab,
		PlatformHost: c.host,
		Capability:   string(platform.ArchiveCapabilityHistoricalIssues),
		Err: errors.New(
			"server answered a keyset pagination request with offset pagination; " +
				"issue inventory traversal requires GitLab 18.3 or later",
		),
	}
}

// gitLabKeysetPage assembles a canonical page from one keyset response:
// exhaustion when the provider issued no continuation, otherwise a cursor
// carrying only the provider's keyset continuation token.
func gitLabKeysetPage[T any](
	items []T,
	cursor gitLabPageCursor,
	token string,
) (platform.Page[T], error) {
	if token == "" {
		return platform.Page[T]{Items: items, Exhausted: true}, nil
	}
	cursor.KeysetCursor = token
	next, err := encodeGitLabPageCursor(cursor)
	if err != nil {
		return platform.Page[T]{}, err
	}
	return platform.Page[T]{Items: items, NextCursor: next}, nil
}

func nextGitLabPage(resp *gitlab.Response) int64 {
	if resp == nil {
		return 0
	}
	return resp.NextPage
}

// collectGitLabPages drains a numeric-page provider list through the bounded
// neutral collector, so every whole-dataset read shares one cycle-guarded,
// page-capped drain instead of a hand-rolled loop.
func collectGitLabPages[T any](
	ctx context.Context,
	fetch func(context.Context, int64) ([]T, int64, error),
) ([]T, error) {
	return platform.CollectPages(ctx, "1", func(ctx context.Context, cursor string) (platform.Page[T], error) {
		pageNumber, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return platform.Page[T]{}, fmt.Errorf("parse GitLab page cursor: %w", err)
		}
		items, nextPage, err := fetch(ctx, pageNumber)
		if err != nil {
			return platform.Page[T]{}, err
		}
		if nextPage == 0 {
			return platform.Page[T]{Items: items, Exhausted: true}, nil
		}
		return platform.Page[T]{Items: items, NextCursor: strconv.FormatInt(nextPage, 10)}, nil
	})
}

// listIssueDiscussionsPage is the single owner of the issue discussions
// endpoint request shape. Callers map errors onto their dataset capability.
func (c *Client) listIssueDiscussionsPage(
	ctx context.Context,
	pid any,
	ref platform.RepoRef,
	number int,
	page int64,
) ([]*gitlab.Discussion, int64, error) {
	discussions, resp, err := c.api.Discussions.ListIssueDiscussions(
		pid, int64(number),
		&gitlab.ListIssueDiscussionsOptions{Page: page, PerPage: defaultPageSize},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, 0, c.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureIssues,
			"list_issue_discussions", err,
		)
	}
	return discussions, nextGitLabPage(resp), nil
}

func (c *Client) listIssueRelatedMergeRequests(
	ctx context.Context,
	pid any,
	ref platform.RepoRef,
	number int,
) ([]*gitlab.BasicMergeRequest, error) {
	return collectGitLabPages(ctx, func(ctx context.Context, page int64) ([]*gitlab.BasicMergeRequest, int64, error) {
		items, resp, err := c.api.Issues.ListMergeRequestsRelatedToIssue(
			pid,
			int64(number),
			&gitlab.ListMergeRequestsRelatedToIssueOptions{
				Page: page, PerPage: defaultPageSize,
			},
			gitlab.WithContext(ctx),
		)
		if err != nil {
			return nil, 0, c.repositoryFeatureError(
				ctx, ref, platform.RepositoryFeatureIssues,
				"list_issue_related_merge_requests", err,
			)
		}
		return items, nextGitLabPage(resp), nil
	})
}

// listMergeRequestDiscussionsPage is the single owner of the merge-request
// discussions endpoint request shape, feeding the ordinary-comment filter, the
// review-thread extraction, and the live event surfaces. Callers map errors
// onto their dataset capability.
func (c *Client) listMergeRequestDiscussionsPage(
	ctx context.Context,
	pid any,
	ref platform.RepoRef,
	number int,
	page int64,
) ([]*gitlab.Discussion, int64, error) {
	discussions, resp, err := c.api.Discussions.ListMergeRequestDiscussions(
		pid, int64(number),
		&gitlab.ListMergeRequestDiscussionsOptions{Page: page, PerPage: defaultPageSize},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, 0, c.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests,
			"list_merge_request_discussions", err,
		)
	}
	return discussions, nextGitLabPage(resp), nil
}

func (c *Client) decodePageCursor(
	ref platform.RepoRef,
	number int,
	encoded string,
	mode string,
	since time.Time,
	shape gitLabCursorShape,
) (gitLabPageCursor, error) {
	repoPath, err := rawProjectPath(ref)
	if err != nil {
		return gitLabPageCursor{}, err
	}
	repoPath = strings.Trim(repoPath, "/")
	expectedSince := ""
	if !since.IsZero() {
		expectedSince = since.UTC().Format(time.RFC3339Nano)
	}
	if encoded == "" {
		cursor := gitLabPageCursor{
			Mode: mode, Host: ref.Host, RepoPath: repoPath, Number: number, Since: expectedSince,
		}
		if shape != gitLabCursorKeyset {
			cursor.Page = 1
		}
		return cursor, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return gitLabPageCursor{}, c.pageCursorError(err)
	}
	var cursor gitLabPageCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return gitLabPageCursor{}, c.pageCursorError(err)
	}
	if cursor.Mode != mode || cursor.Host != ref.Host || cursor.RepoPath != repoPath || cursor.Number != number ||
		cursor.Since != expectedSince {
		return gitLabPageCursor{}, c.pageCursorError(errors.New("cursor does not match page enumeration"))
	}
	if err := validateGitLabCursorShape(cursor, shape); err != nil {
		return gitLabPageCursor{}, c.pageCursorError(err)
	}
	return cursor, nil
}

// validateGitLabCursorShape rejects a resumption cursor whose continuation
// state does not belong to the traversal kind decoding it, so an offset page
// number can never be replayed as a keyset token or vice versa.
func validateGitLabCursorShape(cursor gitLabPageCursor, shape gitLabCursorShape) error {
	switch shape {
	case gitLabCursorKeyset:
		if cursor.KeysetCursor == "" || cursor.Page != 0 {
			return errors.New("cursor does not carry keyset continuation state")
		}
	default:
		if cursor.Page <= 0 || cursor.KeysetCursor != "" {
			return errors.New("cursor does not carry offset continuation state")
		}
	}
	return nil
}

func (c *Client) pageCursorError(err error) error {
	return platform.ProviderContract(platform.KindGitLab, c.host, "archive_cursor", err)
}

func encodeGitLabPageCursor(cursor gitLabPageCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode GitLab page cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var (
	_ platform.IssuePageReader        = (*Client)(nil)
	_ platform.MergeRequestPageReader = (*Client)(nil)
)
