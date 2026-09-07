package gitealike

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/platform"
)

// pageCursor binds an opaque resumable cursor to the enumeration that produced
// it: the query mode, the repository identity, the item number for detail
// datasets, and the maintenance watermark. Reusing a cursor with a different
// shape is a provider-contract violation. Before pins the wall-clock upper
// bound of the traversal so restarts do not re-deliver items created or
// updated after the scan began.
type pageCursor struct {
	Mode   string `json:"mode"`
	Repo   string `json:"repo"`
	Number int    `json:"number,omitempty"`
	Page   int    `json:"page"`
	Since  string `json:"since,omitempty"`
	Before string `json:"before,omitempty"`
	// Boundary is the newest creation time a created-order traversal has
	// covered, RFC3339Nano UTC. The list endpoints cannot enforce a
	// page-spanning tie-break, so per the ItemOrderCreated contract the
	// traversal resumes inclusively at this timestamp: equal-created items at
	// a page seam are re-delivered instead of trusted to keep their page
	// position, and consumers absorb the overlap through idempotent upserts.
	Boundary string `json:"boundary,omitempty"`
	// Sweep marks an in-progress boundary re-delivery pass and records the
	// page that triggered it. The sweep steps into the already-consumed
	// direction while pages still open on the boundary timestamp,
	// re-delivering the boundary tie group, then returns to the trigger page.
	Sweep int `json:"sweep,omitempty"`
	// Swept records the page whose boundary sweep just completed so the
	// trigger page's own delivery does not re-trigger the same sweep.
	Swept int `json:"swept,omitempty"`
}

// ListIssuesPage handles archive inventory and maintenance scans.
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
		return p.listInventoryIssuesPage(ctx, ref, query.Order, since, query.Cursor, "updated_issues")
	}
	return p.listInventoryIssuesPage(ctx, ref, query.Order, time.Time{}, query.Cursor, "historical_issues")
}

// ListMergeRequestsPage is the merge-request counterpart to ListIssuesPage.
// The pulls endpoint has stable sort modes, so created-order traversal pages
// forward under "oldest" and the maintenance traversal pages "recentupdate"
// descending until it crosses the overlapped watermark.
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
		return p.listInventoryMergeRequestsPage(ctx, ref, query.Order, since, query.Cursor, "updated_merge_requests")
	}
	return p.listInventoryMergeRequestsPage(ctx, ref, query.Order, time.Time{}, query.Cursor, "historical_merge_requests")
}

// listInventoryIssuesPage owns the historical and maintenance issue request
// shapes over the archive transport. The requested order is threaded
// explicitly: created-order runs the boundary-inclusive backward walk, while
// updated-order keeps updated-time traversal semantics whether or not a
// watermark is present, applying the inclusive watermark through the
// endpoint's since filter only when one was requested.
func (p *Provider) listInventoryIssuesPage(
	ctx context.Context,
	ref platform.RepoRef,
	order platform.ItemOrder,
	since time.Time,
	encoded string,
	mode string,
) (platform.Page[platform.Issue], error) {
	t, err := p.archiveTransport()
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	cursor, err := p.decodePageCursor(ref, 0, mode, since, encoded)
	if err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	items, page, err := t.ListIssuesPage(ctx, ref, ArchiveListOptions{
		Page: cursor.Page, PageSize: defaultPageSize,
		Since: inclusiveWatermark(since), Before: parseCursorTime(cursor.Before),
	})
	if err != nil {
		return platform.Page[platform.Issue]{}, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureIssues, err,
		)
	}
	// The Gitea-compatible issue endpoint has no sort parameter and returns
	// newest pages first. The first request of a fresh traversal discovers the
	// final page; each subsequent request walks backward and reverses that
	// page. The discovery jump is gated on the fresh call: a resumed cursor at
	// page one is the final backward step and must consume, not re-discover,
	// or the walk would cycle 1 -> last forever.
	if encoded == "" && cursor.Page == 1 && page.Last > 1 {
		cursor.Page = page.Last
		return nextCursorPage([]platform.Issue(nil), cursor, true)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Created.Equal(items[j].Created) {
			return items[i].Index < items[j].Index
		}
		return items[i].Created.Before(items[j].Created)
	})
	if order == platform.ItemOrderCreated {
		return createdOrderIssuesPage(ref, items, cursor)
	}
	out := normalizeIssuePage(ref, items, time.Time{})
	if cursor.Page <= 1 {
		return platform.Page[platform.Issue]{Items: out, Exhausted: true}, nil
	}
	cursor.Page--
	return nextCursorPage(out, cursor, len(out) == 0)
}

// createdOrderIssuesPage advances the created-order backward walk. The issue
// endpoint has no stable tie-break across page boundaries, so the cursor
// tracks the newest creation time the walk has covered and resumption is
// inclusive at it: when the next page still opens on the boundary timestamp,
// the walk sweeps back through the already-consumed pages that hold the tie
// group and re-delivers it before continuing, so an interrupted scan never
// permanently skips an equal-created item the provider re-ordered across the
// seam. Consumers absorb the overlap through idempotent upserts.
func createdOrderIssuesPage(
	ref platform.RepoRef,
	items []IssueDTO,
	cursor pageCursor,
) (platform.Page[platform.Issue], error) {
	boundary := parseCursorTime(cursor.Boundary)
	opensOnBoundary := len(items) > 0 && !boundary.IsZero() && items[0].Created.Equal(boundary)
	if cursor.Sweep > 0 {
		out := normalizeIssuePage(ref, items, boundary)
		if opensOnBoundary {
			// The whole page sits inside the boundary tie group, which may
			// extend one page further into the consumed region.
			cursor.Page++
			return nextCursorPage(out, cursor, len(out) == 0)
		}
		cursor.Page = cursor.Sweep
		cursor.Swept = cursor.Sweep
		cursor.Sweep = 0
		return nextCursorPage(out, cursor, len(out) == 0)
	}
	if opensOnBoundary && cursor.Page != cursor.Swept {
		cursor.Sweep = cursor.Page
		cursor.Swept = 0
		cursor.Page++
		return nextCursorPage([]platform.Issue(nil), cursor, true)
	}
	out := normalizeIssuePage(ref, items, boundary)
	if len(items) > 0 {
		if newest := items[len(items)-1].Created; newest.After(boundary) {
			cursor.Boundary = newest.UTC().Format(time.RFC3339Nano)
		}
	}
	cursor.Swept = 0
	if cursor.Page <= 1 {
		return platform.Page[platform.Issue]{Items: out, Exhausted: true}, nil
	}
	cursor.Page--
	return nextCursorPage(out, cursor, len(out) == 0)
}

// normalizeIssuePage filters interleaved pull requests out of an issue page
// and, when a created-order boundary is set, drops items strictly before it:
// those were delivered by earlier pages, while boundary-equal items are
// re-delivered per the inclusive-resumption contract.
func normalizeIssuePage(
	ref platform.RepoRef,
	items []IssueDTO,
	boundary time.Time,
) []platform.Issue {
	out := make([]platform.Issue, 0, len(items))
	for _, item := range items {
		if item.IsPullRequest {
			continue
		}
		if !boundary.IsZero() && item.Created.Before(boundary) {
			continue
		}
		out = append(out, NormalizeIssue(ref, item))
	}
	return out
}

// listInventoryMergeRequestsPage owns the historical and maintenance
// merge-request request shapes over the archive transport. The requested
// order picks the traversal: created-order pages forward under the "oldest"
// sort with boundary-inclusive resumption, while updated-order always pages
// "recentupdate" descending — even without a watermark — bounding each item
// by its update time against the scan-start pin and stopping once a page
// crosses the overlapped inclusive watermark when one was requested.
func (p *Provider) listInventoryMergeRequestsPage(
	ctx context.Context,
	ref platform.RepoRef,
	order platform.ItemOrder,
	since time.Time,
	encoded string,
	mode string,
) (platform.Page[platform.MergeRequest], error) {
	t, err := p.archiveTransport()
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	cursor, err := p.decodePageCursor(ref, 0, mode, since, encoded)
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, err
	}
	sortMode := "oldest"
	if order == platform.ItemOrderUpdated {
		sortMode = "recentupdate"
	}
	items, page, err := t.ListPullRequestsPage(ctx, ref, ArchiveListOptions{
		Page: cursor.Page, PageSize: defaultPageSize, Sort: sortMode,
	})
	if err != nil {
		return platform.Page[platform.MergeRequest]{}, p.repositoryFeatureError(
			ctx, ref, platform.RepositoryFeatureMergeRequests, err,
		)
	}
	if order == platform.ItemOrderCreated {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].Created.Equal(items[j].Created) {
				return items[i].Index < items[j].Index
			}
			return items[i].Created.Before(items[j].Created)
		})
		return createdOrderMergeRequestsPage(ref, items, cursor, page)
	}
	out := make([]platform.MergeRequest, 0, len(items))
	crossedWatermark := false
	overlap := inclusiveWatermark(since)
	pin := parseCursorTime(cursor.Before)
	for _, item := range items {
		if item.Updated.After(pin) {
			continue
		}
		if !since.IsZero() && item.Updated.Before(overlap) {
			crossedWatermark = true
			continue
		}
		out = append(out, NormalizePullRequest(ref, item))
	}
	if page.Next == 0 || crossedWatermark {
		return platform.Page[platform.MergeRequest]{Items: out, Exhausted: true}, nil
	}
	cursor.Page = page.Next
	return nextCursorPage(out, cursor, len(out) == 0)
}

// createdOrderMergeRequestsPage advances the created-order "oldest" forward
// walk. The pulls endpoint has no stable tie-break across page boundaries, so
// the cursor tracks the newest creation time the walk has covered and
// resumption is inclusive at it: when a page still opens on the boundary
// timestamp, the walk sweeps back through the already-consumed pages that
// hold the tie group and re-delivers it before continuing, so an interrupted
// scan never permanently skips an equal-created item the provider re-ordered
// across the seam. Consumers absorb the overlap through idempotent upserts.
func createdOrderMergeRequestsPage(
	ref platform.RepoRef,
	items []PullRequestDTO,
	cursor pageCursor,
	page Page,
) (platform.Page[platform.MergeRequest], error) {
	boundary := parseCursorTime(cursor.Boundary)
	pin := parseCursorTime(cursor.Before)
	opensOnBoundary := len(items) > 0 && !boundary.IsZero() && items[0].Created.Equal(boundary)
	if cursor.Sweep > 0 {
		out := normalizeMergeRequestPage(ref, items, boundary, pin)
		if opensOnBoundary && cursor.Page > 1 {
			// The whole page sits inside the boundary tie group, which may
			// extend one page further into the consumed region.
			cursor.Page--
			return nextCursorPage(out, cursor, len(out) == 0)
		}
		cursor.Page = cursor.Sweep
		cursor.Swept = cursor.Sweep
		cursor.Sweep = 0
		return nextCursorPage(out, cursor, len(out) == 0)
	}
	if opensOnBoundary && cursor.Page > 1 && cursor.Page != cursor.Swept {
		cursor.Sweep = cursor.Page
		cursor.Swept = 0
		cursor.Page--
		return nextCursorPage([]platform.MergeRequest(nil), cursor, true)
	}
	out := normalizeMergeRequestPage(ref, items, boundary, pin)
	for _, item := range slices.Backward(items) {
		created := item.Created
		if created.After(pin) {
			continue
		}
		if created.After(boundary) {
			cursor.Boundary = created.UTC().Format(time.RFC3339Nano)
		}
		break
	}
	cursor.Swept = 0
	if page.Next == 0 {
		return platform.Page[platform.MergeRequest]{Items: out, Exhausted: true}, nil
	}
	cursor.Page = page.Next
	return nextCursorPage(out, cursor, len(out) == 0)
}

// normalizeMergeRequestPage bounds a created-order merge-request page by the
// scan-start pin and, when a boundary is set, drops items strictly before it:
// those were delivered by earlier pages, while boundary-equal items are
// re-delivered per the inclusive-resumption contract.
func normalizeMergeRequestPage(
	ref platform.RepoRef,
	items []PullRequestDTO,
	boundary time.Time,
	pin time.Time,
) []platform.MergeRequest {
	out := make([]platform.MergeRequest, 0, len(items))
	for _, item := range items {
		if item.Created.After(pin) {
			continue
		}
		if !boundary.IsZero() && item.Created.Before(boundary) {
			continue
		}
		out = append(out, NormalizePullRequest(ref, item))
	}
	return out
}

func nextCursorPage[T any](items []T, cursor pageCursor, progressOnly bool) (platform.Page[T], error) {
	next, err := encodePageCursor(cursor)
	if err != nil {
		return platform.Page[T]{}, err
	}
	return platform.Page[T]{Items: items, NextCursor: next, ProgressOnly: progressOnly}, nil
}

func (p *Provider) archiveTransport() (ArchiveTransport, error) {
	t, ok := p.transport.(ArchiveTransport)
	if !ok {
		return nil, platform.UnsupportedCapability(p.kind, p.host, string(platform.ArchiveCapabilityHistoricalIssues))
	}
	return t, nil
}

func (p *Provider) decodePageCursor(ref platform.RepoRef, number int, mode string, since time.Time, encoded string) (pageCursor, error) {
	expected := pageCursor{Mode: mode, Repo: cursorRepoKey(p.kind, p.host, ref), Number: number, Page: 1}
	if !since.IsZero() {
		expected.Since = since.UTC().Format(time.RFC3339Nano)
	}
	if encoded == "" {
		expected.Before = time.Now().UTC().Format(time.RFC3339Nano)
		return expected, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pageCursor{}, p.pageCursorError(err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return pageCursor{}, p.pageCursorError(err)
	}
	if cursor.Mode != expected.Mode || cursor.Repo != expected.Repo || cursor.Number != expected.Number || cursor.Since != expected.Since || cursor.Page < 1 || cursor.Before == "" {
		return pageCursor{}, p.pageCursorError(errors.New("cursor does not match page enumeration"))
	}
	if cursor.Sweep < 0 || cursor.Swept < 0 {
		return pageCursor{}, p.pageCursorError(errors.New("cursor does not match page enumeration"))
	}
	if cursor.Boundary != "" {
		if _, err := time.Parse(time.RFC3339Nano, cursor.Boundary); err != nil {
			return pageCursor{}, p.pageCursorError(err)
		}
	}
	return cursor, nil
}

func (p *Provider) pageCursorError(err error) error {
	return platform.ProviderContract(p.kind, p.host, "archive_cursor", err)
}

func encodePageCursor(cursor pageCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode page cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func cursorRepoKey(kind platform.Kind, host string, ref platform.RepoRef) string {
	return strings.Join([]string{string(kind), host, string(ref.Platform), ref.Host, ref.Owner, ref.Name}, "\x00")
}

func inclusiveWatermark(since time.Time) time.Time {
	if since.IsZero() {
		return time.Time{}
	}
	return since.UTC().Add(-time.Second)
}

func parseCursorTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

// collectTransportPages drains a numeric-page transport list through the
// bounded neutral collector, so every whole-dataset read shares one
// cycle-guarded, page-capped drain instead of a hand-rolled loop. Transport
// errors surface unmapped; callers apply their own error mapping.
func collectTransportPages[T any](
	ctx context.Context,
	fetch func(context.Context, PageOptions) ([]T, Page, error),
) ([]T, error) {
	return platform.CollectPages(ctx, "1", func(ctx context.Context, cursor string) (platform.Page[T], error) {
		pageNumber, err := strconv.Atoi(cursor)
		if err != nil {
			return platform.Page[T]{}, fmt.Errorf("parse gitealike page cursor: %w", err)
		}
		items, next, err := fetch(ctx, PageOptions{Page: pageNumber, PageSize: defaultPageSize})
		if err != nil {
			return platform.Page[T]{}, err
		}
		nextPage := NextPage(next.Next)
		if nextPage == 0 {
			return platform.Page[T]{Items: items, Exhausted: true}, nil
		}
		return platform.Page[T]{Items: items, NextCursor: strconv.Itoa(nextPage)}, nil
	})
}

var (
	_ platform.IssuePageReader        = (*Provider)(nil)
	_ platform.MergeRequestPageReader = (*Provider)(nil)
)
