package githubapp

import (
	"fmt"

	"go.kenn.io/forge/githubapp"
)

const maxAppNameLength = 34

func NewManifest(name, homepageURL, redirectURL string) (githubapp.Manifest, error) {
	if name == "" {
		return githubapp.Manifest{}, fmt.Errorf("app name is required")
	}
	if len(name) > maxAppNameLength {
		return githubapp.Manifest{}, fmt.Errorf(
			"app name %q exceeds GitHub's %d character limit", name, maxAppNameLength,
		)
	}
	if homepageURL == "" {
		homepageURL = DefaultHomepageURL
	}
	return githubapp.Manifest{
		Name:               name,
		URL:                homepageURL,
		HookAttributes:     githubapp.HookAttributes{URL: homepageURL, Active: false},
		RedirectURL:        redirectURL,
		Public:             false,
		DefaultPermissions: DefaultPermissions(),
		DefaultEvents:      []string{},
	}, nil
}
