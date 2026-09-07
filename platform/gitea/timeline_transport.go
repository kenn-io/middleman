package gitea

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// timelineLabelTransport normalizes the timeline label shape emitted by
// supported Gitea 1.24 servers before the SDK decodes it. Those servers return
// a single label object while the pinned SDK accepts only an array.
type timelineLabelTransport struct {
	base http.RoundTripper
}

func (t *timelineLabelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil ||
		req.Method != http.MethodGet ||
		resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices ||
		!strings.HasSuffix(req.URL.Path, "/timeline") {
		return resp, err
	}

	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if normalized, changed := normalizeTimelineLabels(body); changed {
		body = normalized
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func normalizeTimelineLabels(body []byte) ([]byte, bool) {
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(body, &events); err != nil {
		return body, false
	}
	changed := false
	for _, event := range events {
		label, ok := event["label"]
		if !ok {
			continue
		}
		trimmed := bytes.TrimSpace(label)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		event["label"] = append(append([]byte{'['}, trimmed...), ']')
		changed = true
	}
	if !changed {
		return body, false
	}
	normalized, err := json.Marshal(events)
	if err != nil {
		return body, false
	}
	return normalized, true
}
