// Package githubapp implements the GitHub App primitives kenn-forge
// uses to mitigate PAT rate-limit exhaustion: the App Manifest
// creation flow, app JWT signing, and installation access token
// minting. Installation tokens carry their own rate-limit budget
// (5,000+ requests/hour per installation, scaling with repository
// count), separate from any personal access token.
package githubapp

import (
	"encoding/json"
	"fmt"
)

// Manifest is the GitHub App Manifest posted to
// https://HOST/settings/apps/new. GitHub renders a one-click app
// creation page from it and redirects back with a conversion code.
// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type HookAttributes struct {
	URL    string `json:"url,omitempty"`
	Active bool   `json:"active"`
}

func (m Manifest) JSON() (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encoding app manifest: %w", err)
	}
	return string(data), nil
}
