package githubapp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const DefaultHomepageURL = "https://go.kenn.io/forge"

// DefaultPermissions is the permission set kenn-forge's sync surface needs.
// The App is read-only by design: mutations use the user's own credential so
// they remain attributed to that user. Webhooks stay disabled because Forge
// polls providers.
//
// Permission matrix (all read):
//   - contents: PR and issue sync, releases, tags, clone, and fetch
//   - issues: issue lists, details, comments, and timelines
//   - pull_requests: PR lists, details, reviews, and review threads
//   - checks: check runs for refs
//   - statuses: combined commit status
//   - actions: workflow runs awaiting approval
//   - metadata: mandatory baseline for every GitHub App
func DefaultPermissions() map[string]string {
	return map[string]string{
		"actions": "read", "checks": "read", "contents": "read",
		"issues": "read", "metadata": "read", "pull_requests": "read", "statuses": "read",
	}
}

// RandomAppName supplies an editable default within GitHub's name limit.
func RandomAppName() (string, error) {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating app name suffix: %w", err)
	}
	return "kenn-forge-" + hex.EncodeToString(buf[:]), nil
}
