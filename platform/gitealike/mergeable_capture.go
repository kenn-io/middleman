package gitealike

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

const mergeableCaptureMaxBodyBytes = 1 << 20

type MergeableCache struct {
	mu        sync.Mutex
	byHTMLURL map[string]mergeableCacheEntry
}

func NewMergeableCache() *MergeableCache {
	return &MergeableCache{byHTMLURL: make(map[string]mergeableCacheEntry)}
}

func (c *MergeableCache) CapturePullRequestJSON(data []byte) {
	data = bytes.TrimSpace(data)
	if c == nil || len(data) == 0 {
		return
	}
	var items []mergeableCaptureItem
	if err := json.Unmarshal(data, &items); err != nil {
		var item mergeableCaptureItem
		if err := json.Unmarshal(data, &item); err != nil {
			return
		}
		items = []mergeableCaptureItem{item}
	}
	for _, item := range items {
		c.capturePullRequest(item)
	}
}

func (c *MergeableCache) MergeableForPullRequest(htmlURL, headSHA, baseSHA string) (*bool, bool) {
	if c == nil || htmlURL == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byHTMLURL[htmlURL]
	if !ok || !entry.matches(headSHA, baseSHA) {
		return nil, false
	}
	if entry.mergeable == nil {
		return nil, true
	}
	value := *entry.mergeable
	return &value, true
}

type PullRequestMetrics struct {
	Additions      int
	AdditionsKnown bool
	Deletions      int
	DeletionsKnown bool
	FilesChanged   *int
}

func (c *MergeableCache) MetricsForPullRequest(
	htmlURL, headSHA, baseSHA string,
) (PullRequestMetrics, bool) {
	if c == nil || htmlURL == "" {
		return PullRequestMetrics{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byHTMLURL[htmlURL]
	if !ok || !entry.matches(headSHA, baseSHA) || entry.metrics == nil {
		return PullRequestMetrics{}, false
	}
	metrics := *entry.metrics
	metrics.FilesChanged = cloneIntPointer(metrics.FilesChanged)
	return metrics, true
}

func (c *MergeableCache) capturePullRequest(item mergeableCaptureItem) {
	if item.HTMLURL == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.byHTMLURL[item.HTMLURL] = mergeableCacheEntry{
		mergeable: item.mergeableValue(),
		metrics:   item.metricsValue(),
		headSHA:   item.Head.SHA,
		baseSHA:   item.Base.SHA,
	}
}

type mergeableCaptureItem struct {
	HTMLURL   string                 `json:"html_url"`
	Mergeable json.RawMessage        `json:"mergeable"`
	Head      mergeableCaptureBranch `json:"head"`
	Base      mergeableCaptureBranch `json:"base"`
	Additions *int                   `json:"additions"`
	Deletions *int                   `json:"deletions"`
	Changed   *int                   `json:"changed_files"`
}

type mergeableCaptureBranch struct {
	SHA string `json:"sha"`
}

func (item mergeableCaptureItem) mergeableValue() *bool {
	if len(item.Mergeable) == 0 || bytes.Equal(bytes.TrimSpace(item.Mergeable), []byte("null")) {
		return nil
	}
	var value bool
	if err := json.Unmarshal(item.Mergeable, &value); err != nil {
		return nil
	}
	return &value
}

func (item mergeableCaptureItem) metricsValue() *PullRequestMetrics {
	if item.Additions == nil && item.Deletions == nil && item.Changed == nil {
		return nil
	}
	return &PullRequestMetrics{
		Additions:      intPointerValue(item.Additions),
		AdditionsKnown: item.Additions != nil,
		Deletions:      intPointerValue(item.Deletions),
		DeletionsKnown: item.Deletions != nil,
		FilesChanged:   cloneIntPointer(item.Changed),
	}
}

func intPointerValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type mergeableCacheEntry struct {
	mergeable *bool
	metrics   *PullRequestMetrics
	headSHA   string
	baseSHA   string
}

func (e mergeableCacheEntry) matches(headSHA, baseSHA string) bool {
	return e.headSHA != "" && headSHA != "" && e.headSHA == headSHA &&
		e.baseSHA != "" && baseSHA != "" && e.baseSHA == baseSHA
}

type MergeableCaptureTransport struct {
	Base  http.RoundTripper
	Cache *MergeableCache
}

func (t *MergeableCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || t.Cache == nil || !shouldCaptureMergeable(req, resp) {
		return resp, err
	}

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, mergeableCaptureMaxBodyBytes+1))
	if readErr != nil {
		return resp, readErr
	}

	if len(data) > mergeableCaptureMaxBodyBytes {
		resp.Body = &readCloser{
			Reader: io.MultiReader(bytes.NewReader(data), resp.Body),
			close:  resp.Body.Close,
		}
		return resp, nil
	}

	closeErr := resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if closeErr != nil {
		return resp, closeErr
	}
	t.Cache.CapturePullRequestJSON(data)
	return resp, nil
}

func shouldCaptureMergeable(req *http.Request, resp *http.Response) bool {
	if req == nil || req.URL == nil || !isMergeableCaptureMethod(req.Method) || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "application/json") {
		return false
	}
	return isPullRequestAPIPath(req.URL.Path)
}

func isMergeableCaptureMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch:
		return true
	default:
		return false
	}
}

func isPullRequestAPIPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] != "repos" {
			continue
		}
		pullsIndex := i + 3
		if parts[pullsIndex] != "pulls" {
			continue
		}
		return len(parts) == pullsIndex+1 || len(parts) == pullsIndex+2
	}
	return false
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r *readCloser) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}
