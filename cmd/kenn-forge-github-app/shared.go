package main

import (
	"context"
	"errors"
	"fmt"
	appfiles "go.kenn.io/forge/internal/githubapp"
	"strings"
	"time"

	"go.kenn.io/forge/githubapp"
	"go.kenn.io/forge/internal/config"
)

// loadConfig uses the GitHub App management entry point so every command
// applies the same structural validation before changing app state.
func (env *appEnv) loadConfig() (*config.Config, error) {
	cfg, err := config.LoadForGitHubAppRepair(env.configPath)
	if err != nil {
		return nil, fmt.Errorf("loading kenn-forge config %s: %w", env.configPath, err)
	}
	return cfg, nil
}

func (env *appEnv) apiClient(host string) *githubapp.Client {
	if env.apiBase != "" {
		return githubapp.NewClientWithBase(env.apiBase)
	}
	return githubapp.NewClient(host)
}

func (env *appEnv) webBaseFor(host string) string {
	if env.webBase != "" {
		return strings.TrimRight(env.webBase, "/")
	}
	return githubapp.WebBaseForHost(host)
}

// selectApp picks a configured app credential. Same-host configs can carry
// multiple private apps, so management commands must disambiguate by owner or
// app id when a host alone is not unique.
func selectApp(
	cfg *config.Config, host, owner string, appID int64,
) (config.GitHubAppConfig, error) {
	if cfg == nil || len(cfg.GitHubApps) == 0 {
		return config.GitHubAppConfig{}, fmt.Errorf(
			"no github apps configured; run \"kenn-forge-github-app create\" first",
		)
	}
	normalizedHost := ""
	if strings.TrimSpace(host) != "" {
		var err error
		normalizedHost, err = normalizeHostFlag(host)
		if err != nil {
			return config.GitHubAppConfig{}, err
		}
	}
	owner = strings.TrimSpace(owner)
	var matches []config.GitHubAppConfig
	for _, app := range cfg.GitHubApps {
		if normalizedHost != "" && app.Host != normalizedHost {
			continue
		}
		if appID != 0 && app.AppID != appID {
			continue
		}
		if owner != "" &&
			!strings.EqualFold(app.Owner, owner) &&
			!strings.EqualFold(app.InstallationAccount, owner) {
			continue
		}
		matches = append(matches, app)
	}
	if len(matches) == 0 {
		if normalizedHost != "" && owner != "" {
			return config.GitHubAppConfig{}, fmt.Errorf(
				"no github app configured for host %q and owner %q", normalizedHost, owner,
			)
		}
		if normalizedHost != "" && appID != 0 {
			return config.GitHubAppConfig{}, fmt.Errorf(
				"no github app configured for host %q and app id %d", normalizedHost, appID,
			)
		}
		if normalizedHost != "" {
			return config.GitHubAppConfig{}, fmt.Errorf("no github app configured for host %q", normalizedHost)
		}
		if appID != 0 {
			return config.GitHubAppConfig{}, fmt.Errorf("no github app configured for app id %d", appID)
		}
		return config.GitHubAppConfig{}, fmt.Errorf("no github app configured for owner %q", owner)
	}
	if len(matches) > 1 {
		return config.GitHubAppConfig{}, fmt.Errorf(
			"multiple github apps match; pass --owner or --app-id to select one",
		)
	}
	return matches[0], nil
}

func appJWT(app config.GitHubAppConfig, now time.Time) (string, error) {
	key, err := appfiles.LoadPrivateKey(app.PrivateKeyPath)
	if err != nil {
		return "", err
	}
	return githubapp.SignAppJWT(app.AppID, key, now)
}

// settingsURL is the app's GitHub management page; deletion lives
// under /advanced. Org-owned apps nest under the organization.
func settingsURL(webBase string, app config.GitHubAppConfig) string {
	if strings.EqualFold(app.OwnerType, "Organization") {
		return fmt.Sprintf(
			"%s/organizations/%s/settings/apps/%s", webBase, app.Owner, app.Slug,
		)
	}
	return fmt.Sprintf("%s/settings/apps/%s", webBase, app.Slug)
}

func installURL(webBase string, app config.GitHubAppConfig) string {
	return fmt.Sprintf("%s/apps/%s/installations/new", webBase, app.Slug)
}

// updateAppInConfig replaces the matching app credential and saves. Same-host
// entries are distinct apps, usually one private app per GitHub account.
func updateAppInConfig(
	cfg *config.Config, configPath string, app config.GitHubAppConfig,
) error {
	for i := range cfg.GitHubApps {
		existing := cfg.GitHubApps[i]
		if existing.Host != app.Host {
			continue
		}
		if existing.AppID == app.AppID {
			cfg.GitHubApps[i] = app
			return cfg.Save(configPath)
		}
	}
	cfg.GitHubApps = append(cfg.GitHubApps, app)
	return cfg.Save(configPath)
}

func updateAppSlotInConfig(
	cfg *config.Config,
	configPath string,
	oldApp config.GitHubAppConfig,
	newApp config.GitHubAppConfig,
) error {
	for i := range cfg.GitHubApps {
		existing := cfg.GitHubApps[i]
		if existing.Host == oldApp.Host &&
			existing.AppID == oldApp.AppID &&
			strings.EqualFold(existing.InstallationAccount, oldApp.InstallationAccount) {
			cfg.GitHubApps[i] = newApp
			return cfg.Save(configPath)
		}
	}
	return updateAppInConfig(cfg, configPath, newApp)
}

func removeAppFromConfig(cfg *config.Config, configPath string, app config.GitHubAppConfig) error {
	kept := cfg.GitHubApps[:0]
	for _, existing := range cfg.GitHubApps {
		if existing.Host == app.Host && existing.AppID == app.AppID {
			continue
		}
		kept = append(kept, existing)
	}
	cfg.GitHubApps = kept
	return cfg.Save(configPath)
}

// missingSelectedRepos lists configured github repos on host owned by
// account that a "selected repositories" installation cannot reach,
// given the full names its token reported accessible. Repos with
// their own credential override never resolve to the app token and
// are exempt; callers apply the role-specific policy to the result.
func missingSelectedRepos(
	cfg *config.Config, host, account string, accessible []string,
) []string {
	reachable := make(map[string]struct{}, len(accessible))
	for _, name := range accessible {
		reachable[strings.ToLower(name)] = struct{}{}
	}
	var missing []string
	for _, r := range cfg.Repos {
		if r.PlatformOrDefault() != "github" || r.PlatformHostOrDefault() != host {
			continue
		}
		if r.TokenEnv != "" || r.TokenFile != "" {
			continue
		}
		if !strings.EqualFold(r.Owner, account) {
			continue
		}
		full := r.Owner + "/" + r.Name
		if r.HasNameGlob() {
			missing = append(missing, full+" (glob patterns need an \"All repositories\" install)")
			continue
		}
		if _, ok := reachable[strings.ToLower(full)]; !ok {
			missing = append(missing, full)
		}
	}
	return missing
}

// errPollDeadline is the sentinel a pollTimeoutError matches via Is, so
// callers can recognize a clean deadline with errors.Is(err,
// errPollDeadline) without the marker text leaking into the user-facing
// message. It marks a pollUntil return that ended on its own timeout
// deadline, as opposed to a probe error or context cancellation: callers
// that recover from a clean "nothing appeared in time" (for example
// adopting an existing installation when no new one shows up) must match
// this so a transient probe failure or interrupt surfaces instead of
// being treated as a timeout.
var errPollDeadline = errors.New("poll deadline reached")

// pollTimeoutError is pollUntil's deadline result. Its message stays the
// plain "timed out after <d>" the CLIs have always shown, while Is
// reports a match against errPollDeadline so recovery paths can branch on
// a clean timeout without changing what the user sees.
type pollTimeoutError struct{ timeout time.Duration }

func (e pollTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s", e.timeout)
}

func (e pollTimeoutError) Is(target error) bool {
	return target == errPollDeadline
}

// pollUntil runs probe at the env's poll interval until it reports
// done, the context ends, or timeout elapses. A timeout returns a
// pollTimeoutError (errors.Is(err, errPollDeadline) is true); probe
// errors and context cancellation are returned as-is so callers can tell
// them apart.
func (env *appEnv) pollUntil(
	ctx context.Context,
	timeout time.Duration,
	probe func(context.Context) (bool, error),
) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(env.pollInterval)
	defer ticker.Stop()
	for {
		done, err := probe(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return pollTimeoutError{timeout: timeout}
		case <-ticker.C:
		}
	}
}

func normalizeHostFlag(host string) (string, error) {
	host = strings.TrimSpace(host)
	normalized, err := config.NormalizePlatformHost("github", host)
	if err != nil {
		return "", err
	}
	return normalized, nil
}
