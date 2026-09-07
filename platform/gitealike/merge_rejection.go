package gitealike

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

const mergeRejectionMaxMessageBytes = 512

// MergeRejection is a non-2xx provider response to a merge request,
// captured at the HTTP layer because the Gitea and Forgejo SDKs report
// merge failures as merged=false with a nil error and an unread,
// already-closed body. Without the capture the provider cannot tell a
// rejected merge (head out of date, merge conflict, permissions) from
// a successful one, let alone classify it.
type MergeRejection struct {
	StatusCode int
	Message    string
}

// MergeRejectionCapture holds the most recent rejection seen by the
// paired transport. Requests through a provider transport are
// serialized, so a single slot keyed to nothing is race-free within a
// merge call: Take once before the SDK call to drop stale state, and
// once after a merged=false result to read this call's rejection.
type MergeRejectionCapture struct {
	mu   sync.Mutex
	last *MergeRejection
}

func NewMergeRejectionCapture() *MergeRejectionCapture {
	return &MergeRejectionCapture{}
}

// Take returns the most recent captured rejection and clears it.
func (c *MergeRejectionCapture) Take() *MergeRejection {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	last := c.last
	c.last = nil
	return last
}

func (c *MergeRejectionCapture) record(statusCode int, body []byte) {
	if c == nil {
		return
	}
	message := ""
	var payload struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &payload) == nil {
		message = strings.TrimSpace(payload.Message)
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if len(message) > mergeRejectionMaxMessageBytes {
		message = message[:mergeRejectionMaxMessageBytes]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = &MergeRejection{StatusCode: statusCode, Message: message}
}

// MergeRejectionError converts a captured rejection (or, absent one,
// the bare response status) into the typed HTTP error a failed merge
// must surface.
func MergeRejectionError(rejection *MergeRejection, fallbackStatusCode int) error {
	if rejection != nil {
		return &HTTPError{StatusCode: rejection.StatusCode, Message: rejection.Message}
	}
	return &HTTPError{StatusCode: fallbackStatusCode, Message: "provider did not perform the merge"}
}

// MergeRejectionCaptureTransport snapshots non-2xx responses to the
// pull request merge endpoint into Capture, restoring the body for any
// downstream reader.
type MergeRejectionCaptureTransport struct {
	Base    http.RoundTripper
	Capture *MergeRejectionCapture
}

func (t *MergeRejectionCaptureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || t.Capture == nil || !shouldCaptureMergeRejection(req, resp) {
		return resp, err
	}

	data, readErr := io.ReadAll(io.LimitReader(resp.Body, mergeableCaptureMaxBodyBytes+1))
	if readErr != nil {
		return resp, readErr
	}
	closeErr := resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if closeErr != nil {
		return resp, closeErr
	}
	if len(data) <= mergeableCaptureMaxBodyBytes {
		t.Capture.record(resp.StatusCode, data)
	} else {
		t.Capture.record(resp.StatusCode, nil)
	}
	return resp, nil
}

func shouldCaptureMergeRejection(req *http.Request, resp *http.Response) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodPost || resp.StatusCode < 400 {
		return false
	}
	return isPullRequestMergePath(req.URL.Path)
}

// isPullRequestMergePath matches .../repos/{owner}/{repo}/pulls/{n}/merge.
func isPullRequestMergePath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+5 < len(parts); i++ {
		if parts[i] == "repos" && parts[i+3] == "pulls" && parts[i+5] == "merge" && len(parts) == i+6 {
			return true
		}
	}
	return false
}
