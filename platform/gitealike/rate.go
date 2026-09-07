package gitealike

import (
	"go.kenn.io/forge/platform"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateFromHeaders returns provider rate state only when the response contains
// the complete tuple required to reason about an active quota window.
func RateFromHeaders(header http.Header) (platform.Rate, bool) {
	limit, ok := rateHeaderInt(header, "RateLimit-Limit", "X-RateLimit-Limit")
	if !ok || limit <= 0 {
		return platform.Rate{}, false
	}
	remaining, ok := rateHeaderInt(header, "RateLimit-Remaining", "X-RateLimit-Remaining")
	if !ok || remaining < 0 {
		return platform.Rate{}, false
	}
	resetUnix, ok := rateHeaderInt64(header, "RateLimit-Reset", "X-RateLimit-Reset")
	if !ok || resetUnix <= 0 {
		return platform.Rate{}, false
	}
	return platform.Rate{
		Limit: limit, Remaining: remaining, Reset: time.Unix(resetUnix, 0).UTC(),
	}, true
}

func rateHeaderInt(header http.Header, names ...string) (int, bool) {
	for _, name := range names {
		value, err := strconv.Atoi(strings.TrimSpace(header.Get(name)))
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func rateHeaderInt64(header http.Header, names ...string) (int64, bool) {
	for _, name := range names {
		value, err := strconv.ParseInt(strings.TrimSpace(header.Get(name)), 10, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}
