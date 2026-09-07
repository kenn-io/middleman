package github

import (
	"errors"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	platformgithub "go.kenn.io/forge/platform/github"
)

const etagTTL = 30 * time.Minute

// etagEligibleListPath matches list endpoints that return a collection
// ETag. Supports both github.com (`/repos/{owner}/{name}/{pulls,issues}`)
// and GitHub Enterprise (`/api/v3/repos/...`), since GHE clients route
// through the same RoundTripper.
var etagEligibleListPath = regexp.MustCompile(
	`^(?:/api/v3)?/repos/[^/]+/[^/]+/(pulls|issues)$`,
)

var etagEligibleCommentPath = regexp.MustCompile(
	`^(?:/api/v3)?/repos/[^/]+/[^/]+/issues/[0-9]+/comments$`,
)

type etagEntry struct {
	etag     string
	cachedAt time.Time
}

type etagTransport struct {
	base  http.RoundTripper
	cache sync.Map // URL string -> etagEntry
}

func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("nil request")
	}

	// Gate: only GET requests to allowlisted endpoints
	if req.Method != http.MethodGet || !isETagEligible(req.URL.Path) {
		return t.base.RoundTrip(req)
	}
	if platformgithub.UnconditionalRead(req.Context()) {
		return t.base.RoundTrip(req)
	}

	// Skip later pages
	if page := req.URL.Query().Get("page"); page != "" && page != "1" {
		return t.base.RoundTrip(req)
	}

	url := req.URL.String()

	// Check cache
	if val, ok := t.cache.Load(url); ok {
		entry := val.(etagEntry)
		if time.Since(entry.cachedAt) < etagTTL {
			req = req.Clone(req.Context())
			req.Header.Set("If-None-Match", entry.etag)
		} else {
			t.cache.Delete(url)
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		etag := resp.Header.Get("ETag")
		if responseHasPaginationLink(resp) {
			// A page validator does not prove the complete paginated
			// collection is unchanged. Evict instead of allowing a later
			// 304 for any page to leave the collection without a body.
			t.cache.Delete(url)
		} else if etag != "" {
			t.cache.Store(url, etagEntry{etag: etag, cachedAt: time.Now()})
		} else {
			// No ETag — drop any stale validator so the next request
			// fetches fresh data instead of asserting an out-of-date
			// If-None-Match.
			t.cache.Delete(url)
		}
	case http.StatusNotModified:
		// Deliberately do NOT refresh cachedAt here. The TTL must
		// expire periodically so we issue an unconditional fetch
		// that can detect list growth beyond page 1 (a single-page
		// list that grows to two pages returns 304 for page 1 since
		// the first page content is unchanged). One unconditional
		// fetch per TTL window is a cheap safety net.
	}

	return resp, nil
}

func responseHasPaginationLink(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	for _, value := range resp.Header.Values("Link") {
		for part := range strings.SplitSeq(value, ",") {
			if strings.TrimSpace(part) != "" {
				return true
			}
		}
	}
	return false
}

func isETagEligible(path string) bool {
	return etagEligibleListPath.MatchString(path) ||
		etagEligibleCommentPath.MatchString(path)
}

// invalidateRepo drops cached ETag entries for the given repo's list
// endpoints. The endpoints parameter selects which to invalidate —
// "pulls", "issues", or "comments" — and an empty slice clears every
// supported repo-scoped list path. Used by the sync engine to force an
// unconditional refetch after a partial failure so the next cycle
// re-applies per-item state that the previous cycle failed to persist.
func (t *etagTransport) invalidateRepo(owner, name string, endpoints ...string) {
	base := "/repos/" + owner + "/" + name + "/"
	gheBase := "/api/v3/repos/" + owner + "/" + name + "/"
	t.cache.Range(func(k, _ any) bool {
		urlStr, ok := k.(string)
		if !ok {
			return true
		}
		parsed, err := urlpkg.Parse(urlStr)
		if err != nil {
			return true
		}
		if matchesInvalidateEndpoint(parsed.Path, base, gheBase, endpoints) {
			t.cache.Delete(k)
		}
		return true
	})
}

func matchesInvalidateEndpoint(path, base, gheBase string, endpoints []string) bool {
	if len(endpoints) == 0 {
		// Empty means invalidate every supported repo-scoped list path
		// so callers can recover from partial-failure syncs without
		// needing to enumerate every endpoint we cache.
		return path == base+"pulls" || path == gheBase+"pulls" ||
			path == base+"issues" || path == gheBase+"issues" ||
			isCommentListPath(path, base, gheBase)
	}
	var exactPrefixes []string
	for _, ep := range endpoints {
		switch ep {
		case "comments":
			if isCommentListPath(path, base, gheBase) {
				return true
			}
		default:
			exactPrefixes = append(exactPrefixes, base+ep, gheBase+ep)
		}
	}
	return slices.Contains(exactPrefixes, path)
}

func isCommentListPath(path, base, gheBase string) bool {
	for _, prefix := range []string{base + "issues/", gheBase + "issues/"} {
		if strings.HasPrefix(path, prefix) && strings.HasSuffix(path, "/comments") {
			return true
		}
	}
	return false
}
