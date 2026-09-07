package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	platformgithub "go.kenn.io/forge/platform/github"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestETagTransport_StoresETagOn200(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"abc123"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos/owner/name/pulls", nil)
	resp, err := et.RoundTrip(req)
	require.NoError(err)
	assert.Equal(200, resp.StatusCode)

	// Second request should include If-None-Match
	req2, _ := http.NewRequest("GET", "https://api.github.com/repos/owner/name/pulls", nil)
	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Equal(`"abc123"`, r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.WriteHeader(304)
		return rec.Result(), nil
	})
	resp2, err := et.RoundTrip(req2)
	require.NoError(err)
	assert.Equal(304, resp2.StatusCode)
}

// TestETagTransport_304PreservesCachedAt verifies that a 304 does
// NOT refresh cachedAt. The TTL must expire periodically so the
// transport issues an unconditional fetch that detects list growth
// beyond page 1.
func TestETagTransport_304PreservesCachedAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	url := "https://api.github.com/repos/o/n/pulls"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(304)
		return rec.Result(), nil
	})}

	oldCachedAt := time.Now().Add(-10 * time.Minute)
	et.cache.Store(url, etagEntry{etag: `"e1"`, cachedAt: oldCachedAt})

	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(err)

	val, ok := et.cache.Load(url)
	require.True(ok)
	got := val.(etagEntry)
	assert.Equal(`"e1"`, got.etag, "etag must be preserved across 304")
	assert.Equal(oldCachedAt, got.cachedAt,
		"cachedAt must NOT be refreshed on 304")
}

func TestETagTransport_DifferentURLsIndependent(t *testing.T) {
	callCount := 0
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		callCount++
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"etag`+r.URL.Path+`"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req1, _ := http.NewRequest("GET", "https://api.github.com/repos/a/b/pulls", nil)
	req2, _ := http.NewRequest("GET", "https://api.github.com/repos/c/d/issues", nil)
	_, _ = et.RoundTrip(req1)
	_, _ = et.RoundTrip(req2)

	val1, _ := et.cache.Load("https://api.github.com/repos/a/b/pulls")
	val2, _ := et.cache.Load("https://api.github.com/repos/c/d/issues")
	assert.NotEqual(t, val1.(etagEntry).etag, val2.(etagEntry).etag)
}

func TestETagTransport_PageGt1BypassesETag(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("If-None-Match"), "page>1 should not send If-None-Match")
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"should-not-cache"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos/o/n/pulls?page=2", nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	_, ok := et.cache.Load(req.URL.String())
	assert.False(t, ok, "page>1 response should not be cached")
}

func TestETagTransport_EmptyETagEvictsCachedEntry(t *testing.T) {
	require := require.New(t)

	url := "https://api.github.com/repos/o/n/pulls"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"first"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	// First request caches an ETag.
	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(err)
	_, ok := et.cache.Load(url)
	require.True(ok, "first response should cache")

	// Second request returns 200 with NO ETag header. Must evict so
	// the next request does not send a stale If-None-Match.
	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(200)
		return rec.Result(), nil
	})
	req2, _ := http.NewRequest("GET", url, nil)
	_, err = et.RoundTrip(req2)
	require.NoError(err)

	_, ok = et.cache.Load(url)
	assert.False(t, ok, "200 without ETag must evict cached entry")
}

func TestETagTransport_MultiPageEvictsPageOneETag(t *testing.T) {
	// First request: single page, cache ETag
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"single"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	url := "https://api.github.com/repos/o/n/pulls"
	req, _ := http.NewRequest("GET", url, nil)
	_, _ = et.RoundTrip(req)
	_, ok := et.cache.Load(url)
	assert.True(t, ok, "single-page ETag should be cached")

	// Second request: multi-page (Link: next), should evict the
	// previous validator so future syncs cannot treat page 1's 304
	// as proof that later pages are unchanged.
	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"multi"`)
		rec.Header().Set("Link", `<https://api.github.com/repos/o/n/pulls?page=2>; rel="next"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})

	req2, _ := http.NewRequest("GET", url, nil)
	_, _ = et.RoundTrip(req2)
	_, ok = et.cache.Load(url)
	assert.False(t, ok, "multi-page page 1 ETag should not be cached")
}

func TestETagTransport_MultiHeaderLinkEvictsETag(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"multi-header"`)
		rec.Header().Add("Link", `<https://api.github.com/repos/o/n/pulls?page=1>; rel="prev"`)
		rec.Header().Add("Link", `<https://api.github.com/repos/o/n/pulls?page=3>; rel="next"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	url := "https://api.github.com/repos/o/n/pulls?page=2"
	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	_, ok := et.cache.Load(url)
	assert.False(t, ok, "any paginated Link header should prevent caching")
}

func TestETagTransport_FinalPageLinkEvictsETag(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"final-page"`)
		rec.Header().Set("Link", `<https://api.github.com/repos/o/n/pulls?page=1>; rel="prev"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	url := "https://api.github.com/repos/o/n/pulls?page=2"
	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	_, ok := et.cache.Load(url)
	assert.False(t, ok, "final paginated pages should not cache validators")
}

func TestETagTransport_NonGETBypassesCache(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"should-not-cache"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		req, _ := http.NewRequest(method, "https://api.github.com/repos/o/n/pulls", nil)
		_, err := et.RoundTrip(req)
		require.NoError(t, err)
	}

	// Nothing should be cached
	count := 0
	et.cache.Range(func(_, _ any) bool { count++; return true })
	assert.Equal(t, 0, count)
}

func TestETagTransport_NonAllowlistedPathBypassesCache(t *testing.T) {
	// Pre-populate cache with the URL to prove gate blocks it
	url := "https://api.github.com/repos/o/n/commits/abc123/status"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"nope"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}
	et.cache.Store(url, etagEntry{etag: `"stale"`, cachedAt: time.Now()})

	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)
}

func TestETagTransport_AllowlistedPathPositiveControl(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"allowed"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req, _ := http.NewRequest("GET", "https://api.github.com/repos/o/n/pulls", nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	_, ok := et.cache.Load("https://api.github.com/repos/o/n/pulls")
	assert.True(t, ok, "allowlisted path should be cached")
}

func TestETagTransport_SingleMultiSingleTransition(t *testing.T) {
	assert := assert.New(t)

	url := "https://api.github.com/repos/o/n/pulls"
	phase := 0
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		switch phase {
		case 0: // single-page
			rec.Header().Set("ETag", `"v1"`)
			rec.WriteHeader(200)
		case 1: // multi-page
			rec.Header().Set("ETag", `"v2"`)
			rec.Header().Set("Link", `<https://api.github.com/repos/o/n/pulls?page=2>; rel="next"`)
			rec.WriteHeader(200)
		case 2: // back to single-page
			rec.Header().Set("ETag", `"v3"`)
			rec.WriteHeader(200)
		}
		return rec.Result(), nil
	})}

	// Phase 0: single-page, should cache
	phase = 0
	req, _ := http.NewRequest("GET", url, nil)
	_, _ = et.RoundTrip(req)
	_, ok := et.cache.Load(url)
	assert.True(ok, "phase 0: single-page should cache")

	// Phase 1: multi-page, should evict page 1's validator.
	phase = 1
	req, _ = http.NewRequest("GET", url, nil)
	_, _ = et.RoundTrip(req)
	_, ok = et.cache.Load(url)
	assert.False(ok, "phase 1: multi-page should not cache page 1")

	// Phase 2: back to single-page, should re-cache
	phase = 2
	req, _ = http.NewRequest("GET", url, nil)
	_, _ = et.RoundTrip(req)
	val, ok := et.cache.Load(url)
	assert.True(ok, "phase 2: single-page again should cache")
	assert.Equal(`"v3"`, val.(etagEntry).etag)
}

func TestETagTransport_TTLDrivenMultiPageDetection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	url := "https://api.github.com/repos/o/n/pulls"
	requestCount := 0
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestCount++
		rec := httptest.NewRecorder()
		if r.Header.Get("If-None-Match") != "" {
			// 304 — cachedAt unchanged, so TTL continues counting
			// down from the original 200.
			rec.WriteHeader(304)
			return rec.Result(), nil
		}
		// Unconditional fetch after TTL — now multi-page
		rec.Header().Set("ETag", `"multi"`)
		rec.Header().Set("Link", `<https://api.github.com/repos/o/n/pulls?page=2>; rel="next"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	// Initial cache with artificially old cachedAt (just under TTL)
	et.cache.Store(url, etagEntry{
		etag:     `"old"`,
		cachedAt: time.Now().Add(-etagTTL + time.Minute),
	})

	// Request with valid cache — sends If-None-Match, gets 304
	req, _ := http.NewRequest("GET", url, nil)
	resp, _ := et.RoundTrip(req)
	assert.Equal(304, resp.StatusCode)

	// Now expire the cache
	et.cache.Store(url, etagEntry{
		etag:     `"old"`,
		cachedAt: time.Now().Add(-etagTTL - time.Minute),
	})

	// Request with expired cache — no If-None-Match, gets 200 multi-page
	req, _ = http.NewRequest("GET", url, nil)
	resp, _ = et.RoundTrip(req)
	assert.Equal(200, resp.StatusCode)

	// Cache should now be empty because a page 1 validator is not enough
	// to prove the complete paginated collection is unchanged.
	_, ok := et.cache.Load(url)
	require.False(ok, "multi-page detection after TTL should evict page 1")
}

func TestETagTransport_ExpiredEntryTreatedAsUncached(t *testing.T) {
	url := "https://api.github.com/repos/o/n/pulls"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("If-None-Match"), "expired entry should not send If-None-Match")
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"fresh"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	// Store expired entry
	et.cache.Store(url, etagEntry{etag: `"old"`, cachedAt: time.Now().Add(-etagTTL - time.Minute)})

	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	val, _ := et.cache.Load(url)
	assert.Equal(t, `"fresh"`, val.(etagEntry).etag)
}

func TestETagTransport_DetailEndpointBypassesCache(t *testing.T) {
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("If-None-Match"), "detail endpoints must bypass cache")
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"should-not-cache"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	for _, url := range []string{
		"https://api.github.com/repos/o/n/pulls/123",
		"https://api.github.com/repos/o/n/pulls/123/files",
		"https://api.github.com/repos/o/n/issues/456",
	} {
		req, _ := http.NewRequest("GET", url, nil)
		_, err := et.RoundTrip(req)
		require.NoError(t, err)
		_, ok := et.cache.Load(url)
		assert.False(t, ok, "%s should not be cached", url)
	}
}

func TestETagTransport_IssueCommentsUseCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	url := "https://api.github.com/repos/o/n/issues/456/comments"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Empty(r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"comment-etag"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(err)

	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		assert.Equal(`"comment-etag"`, r.Header.Get("If-None-Match"))
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusNotModified)
		return rec.Result(), nil
	})
	req2, _ := http.NewRequest(http.MethodGet, url, nil)
	_, err = et.RoundTrip(req2)
	require.NoError(err)
}

// TestETagTransport_304DoesNotExtendCacheLifetime verifies that
// 304s do NOT advance cachedAt, so the TTL eventually expires and
// forces an unconditional fetch. This is the safety net for
// detecting list growth beyond page 1.
func TestETagTransport_304DoesNotExtendCacheLifetime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	url := "https://api.github.com/repos/o/n/pulls"
	unconditionalCount := 0
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		if r.Header.Get("If-None-Match") == "" {
			unconditionalCount++
			rec.Header().Set("ETag", `"e1"`)
			rec.WriteHeader(200)
			return rec.Result(), nil
		}
		rec.WriteHeader(304)
		return rec.Result(), nil
	})}

	// Prime the cache with one unconditional fetch.
	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(err)
	require.Equal(1, unconditionalCount)

	val, ok := et.cache.Load(url)
	require.True(ok, "initial 200 should populate cache")
	original := val.(etagEntry).cachedAt

	// Five consecutive 304s should NOT change cachedAt.
	for i := range 5 {
		req2, _ := http.NewRequest("GET", url, nil)
		resp, err := et.RoundTrip(req2)
		require.NoErrorf(err, "round trip #%d", i)
		require.Equalf(304, resp.StatusCode, "round trip #%d", i)

		val, ok := et.cache.Load(url)
		require.Truef(ok, "cache must persist across 304 #%d", i)
		got := val.(etagEntry).cachedAt
		assert.Equalf(original, got,
			"304 #%d must NOT change cachedAt", i)
	}

	assert.Equal(1, unconditionalCount,
		"304s within TTL must not trigger additional unconditional fetches")
}

// TestETagTransport_GHEPathCaches verifies the matcher accepts the
// /api/v3 prefix used by GitHub Enterprise. Without this, GHE clients
// silently skipped the cache entirely because the regex was anchored
// at /repos/... and GHE requests arrive as /api/v3/repos/....
func TestETagTransport_GHEPathCaches(t *testing.T) {
	assert := assert.New(t)
	url := "https://ghe.example.com/api/v3/repos/o/n/pulls"
	et := &etagTransport{base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"ghe-1"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})}

	req, _ := http.NewRequest("GET", url, nil)
	_, err := et.RoundTrip(req)
	require.NoError(t, err)

	val, ok := et.cache.Load(url)
	assert.True(ok, "GHE path should populate cache")
	entry := val.(etagEntry)
	assert.Equal(`"ghe-1"`, entry.etag)

	// Follow-up request should carry If-None-Match for the GHE URL.
	saw := false
	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		saw = r.Header.Get("If-None-Match") == `"ghe-1"`
		rec := httptest.NewRecorder()
		rec.WriteHeader(304)
		return rec.Result(), nil
	})
	req2, _ := http.NewRequest("GET", url, nil)
	_, err = et.RoundTrip(req2)
	require.NoError(t, err)
	assert.True(saw, "second GHE request must send If-None-Match")

	// Issues path also works under GHE prefix.
	issuesURL := "https://ghe.example.com/api/v3/repos/o/n/issues"
	et.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"ghe-issues"`)
		rec.WriteHeader(200)
		return rec.Result(), nil
	})
	req3, _ := http.NewRequest("GET", issuesURL, nil)
	_, err = et.RoundTrip(req3)
	require.NoError(t, err)
	_, ok = et.cache.Load(issuesURL)
	assert.True(ok, "GHE issues path should populate cache")
}

// TestETagTransport_InvalidateRepo verifies that invalidateRepo
// drops only the entries for the targeted repo and endpoints,
// covering both public github.com and GHE /api/v3 paths, and
// leaves unrelated entries intact.
func TestETagTransport_InvalidateRepo(t *testing.T) {
	assert := assert.New(t)

	seed := func(et *etagTransport) {
		now := time.Now()
		et.cache.Store("https://api.github.com/repos/o/n/pulls", etagEntry{etag: `"1"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/o/n/pulls?state=open", etagEntry{etag: `"2"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/o/n/issues", etagEntry{etag: `"3"`, cachedAt: now})
		et.cache.Store("https://ghe.example.com/api/v3/repos/o/n/pulls", etagEntry{etag: `"4"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/other/other/pulls", etagEntry{etag: `"5"`, cachedAt: now})
	}

	t.Run("all endpoints", func(t *testing.T) {
		et := &etagTransport{}
		seed(et)
		et.invalidateRepo("o", "n", "pulls", "issues")

		for _, u := range []string{
			"https://api.github.com/repos/o/n/pulls",
			"https://api.github.com/repos/o/n/pulls?state=open",
			"https://api.github.com/repos/o/n/issues",
			"https://ghe.example.com/api/v3/repos/o/n/pulls",
		} {
			_, ok := et.cache.Load(u)
			assert.Falsef(ok, "invalidateRepo should drop %s", u)
		}
		_, ok := et.cache.Load("https://api.github.com/repos/other/other/pulls")
		assert.True(ok, "invalidateRepo must not touch unrelated repos")
	})

	t.Run("pulls only", func(t *testing.T) {
		et := &etagTransport{}
		seed(et)
		et.invalidateRepo("o", "n", "pulls")

		for _, u := range []string{
			"https://api.github.com/repos/o/n/pulls",
			"https://api.github.com/repos/o/n/pulls?state=open",
			"https://ghe.example.com/api/v3/repos/o/n/pulls",
		} {
			_, ok := et.cache.Load(u)
			assert.Falsef(ok, "pulls-only should drop %s", u)
		}
		_, ok := et.cache.Load("https://api.github.com/repos/o/n/issues")
		assert.True(ok, "pulls-only should preserve issues cache")
	})

	t.Run("issues only", func(t *testing.T) {
		et := &etagTransport{}
		seed(et)
		et.invalidateRepo("o", "n", "issues")

		_, ok := et.cache.Load("https://api.github.com/repos/o/n/issues")
		assert.False(ok, "issues-only should drop issues cache")

		_, ok = et.cache.Load("https://api.github.com/repos/o/n/pulls")
		assert.True(ok, "issues-only should preserve pulls cache")
		_, ok = et.cache.Load("https://ghe.example.com/api/v3/repos/o/n/pulls")
		assert.True(ok, "issues-only should preserve GHE pulls cache")
	})

	t.Run("empty endpoints clears every supported path", func(t *testing.T) {
		et := &etagTransport{}
		now := time.Now()
		et.cache.Store("https://api.github.com/repos/o/n/pulls", etagEntry{etag: `"1"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/o/n/issues", etagEntry{etag: `"2"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/o/n/issues/7/comments", etagEntry{etag: `"3"`, cachedAt: now})
		et.cache.Store("https://ghe.example.com/api/v3/repos/o/n/issues/9/comments", etagEntry{etag: `"4"`, cachedAt: now})
		et.cache.Store("https://api.github.com/repos/other/other/pulls", etagEntry{etag: `"5"`, cachedAt: now})

		et.invalidateRepo("o", "n")

		for _, u := range []string{
			"https://api.github.com/repos/o/n/pulls",
			"https://api.github.com/repos/o/n/issues",
			"https://api.github.com/repos/o/n/issues/7/comments",
			"https://ghe.example.com/api/v3/repos/o/n/issues/9/comments",
		} {
			_, ok := et.cache.Load(u)
			assert.Falsef(ok, "empty endpoints should drop %s", u)
		}
		_, ok := et.cache.Load("https://api.github.com/repos/other/other/pulls")
		assert.True(ok, "empty endpoints must not touch unrelated repos")
	})
}

func TestIsNotModified(t *testing.T) {
	assert := assert.New(t)

	resp304 := &http.Response{StatusCode: 304}
	err304 := &gh.ErrorResponse{Response: resp304}
	assert.True(platformgithub.IsNotModified(err304))

	resp403 := &http.Response{StatusCode: 403}
	err403 := &gh.ErrorResponse{Response: resp403}
	assert.False(platformgithub.IsNotModified(err403))

	assert.False(platformgithub.IsNotModified(errors.New("random error")))

	errNilResp := &gh.ErrorResponse{Response: nil}
	assert.False(platformgithub.IsNotModified(errNilResp), "nil Response should not panic")
}
