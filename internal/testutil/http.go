package testutil

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// DoJSON serves a JSON request from a loopback client through handler.
func DoJSON(
	t testing.TB,
	handler http.Handler,
	method, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Host = "127.0.0.1:8091"
	req.RemoteAddr = "127.0.0.1:12345"
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
