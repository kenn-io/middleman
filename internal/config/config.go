package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"go.kenn.io/forge/internal/federation"
	platformpkg "go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/tokenauth"
)

const (
	defaultGitHubTokenEnv                  = "KENN_FORGE_GITHUB_TOKEN"
	defaultForgejoTokenEnv                 = "KENN_FORGE_FORGEJO_TOKEN"
	defaultGiteaTokenEnv                   = "KENN_FORGE_GITEA_TOKEN"
	defaultSyncInterval                    = "5m"
	defaultActivePRRefreshInterval         = "2m"
	defaultActivePRWindow                  = "4h"
	defaultNotificationSyncInterval        = "2m"
	defaultNotificationPropagationInterval = "1m"
	defaultHost                            = "127.0.0.1"
	defaultPort                            = 8091
	defaultViewMode                        = "threaded"
	defaultTimeRange                       = "7d"
	defaultBasePath                        = "/"
	defaultSyncBudgetPerHour               = 500
	defaultBranchActivityRetentionDays     = 90
	defaultBranchActivityMaxCommits        = 5000
	DefaultInitialTimelineEntryLimit       = 50
	MinInitialTimelineEntryLimit           = 10
	MaxInitialTimelineEntryLimit           = 250
	defaultPlatform                        = "github"
	defaultPlatformHost                    = platformpkg.DefaultGitHubHost
	defaultSSEBufferSize                   = 256
	defaultMCPDiffCacheMB                  = 128
	minSSEBufferSize                       = 16
	maxSSEBufferSize                       = 16384
)

func githubAppRole(app GitHubAppConfig) string {
	role := strings.ToLower(strings.TrimSpace(app.Role))
	if role == "" {
		return GitHubAppRoleSync
	}
	return role
}

const (
	// GitHubAppRoleSync is the default GitHub App role for ordinary sync and
	// user-facing reads.
	GitHubAppRoleSync = "sync"
	// GitHubAppRoleArchive reserves an installation for historical archive
	// work. Archive requests use this App's independent GitHub quota.
	GitHubAppRoleArchive = "archive"
)

const (
	// IssueWorkspaceBranchStyleSlug appends a slug derived from the
	// issue title onto kenn-forge/issue-<n>, producing recognizable
	// branch names that match common GitHub workflow conventions.
	IssueWorkspaceBranchStyleSlug = "slug"
	// IssueWorkspaceBranchStyleBare keeps the original
	// kenn-forge/issue-<n> form with no title slug appended.
	IssueWorkspaceBranchStyleBare = "bare"

	defaultIssueWorkspaceBranchStyle = IssueWorkspaceBranchStyleSlug
)

type Repo struct {
	Owner            string `toml:"owner" json:"owner"`
	Name             string `toml:"name" json:"name"`
	RepoPath         string `toml:"repo_path,omitempty" json:"repo_path,omitempty"`
	Platform         string `toml:"platform,omitempty" json:"platform,omitempty"`
	PlatformHost     string `toml:"platform_host,omitempty" json:"platform_host,omitempty"`
	PlatformRepoID   string `toml:"platform_repo_id,omitempty" json:"platform_repo_id,omitempty"`
	TokenEnv         string `toml:"token_env,omitempty" json:"token_env,omitempty"`
	TokenFile        string `toml:"token_file,omitempty" json:"token_file,omitempty"`
	WorktreeBasePath string `toml:"worktree_base_path,omitempty" json:"worktree_base_path,omitempty"`
}

type KataProjectRepoMapping struct {
	DaemonID     string `toml:"daemon_id,omitempty" json:"daemon_id,omitempty"`
	ProjectUID   string `toml:"project_uid" json:"project_uid"`
	Provider     string `toml:"provider" json:"provider"`
	PlatformHost string `toml:"platform_host" json:"platform_host"`
	RepoPath     string `toml:"repo_path" json:"repo_path"`
}

// RepoPreset is a named repository scope shown by the global repository
// selector. Global is a built-in UI scope and is never persisted here.
type RepoPreset struct {
	Name  string                 `toml:"name" json:"name"`
	Repos []RepoPresetRepository `toml:"repos" json:"repos" nullable:"false"`
}

// RepoPresetRepository stores provider-verified identity alongside the
// last-known route used for display.
type RepoPresetRepository struct {
	Provider       string `toml:"provider" json:"provider"`
	PlatformHost   string `toml:"platform_host" json:"platform_host"`
	PlatformRepoID string `toml:"platform_repo_id" json:"platform_repo_id"`
	RepoPath       string `toml:"repo_path" json:"repo_path"`
}

func cloneRepoPresets(presets []RepoPreset) []RepoPreset {
	if presets == nil {
		return nil
	}
	out := slices.Clone(presets)
	for i := range out {
		out[i].Repos = slices.Clone(out[i].Repos)
	}
	return out
}

func normalizeRepoPresets(presets []RepoPreset) error {
	seenNames := make(map[string]struct{}, len(presets))
	for i := range presets {
		preset := &presets[i]
		preset.Name = strings.TrimSpace(preset.Name)
		if preset.Name == "" {
			return fmt.Errorf("repo_presets[%d]: name is required", i)
		}
		nameKey := strings.ToLower(preset.Name)
		if nameKey == "global" {
			return fmt.Errorf("repo_presets[%d]: name %q is reserved", i, preset.Name)
		}
		if _, ok := seenNames[nameKey]; ok {
			return fmt.Errorf("config: duplicate repo preset name %q", preset.Name)
		}
		seenNames[nameKey] = struct{}{}

		repos := make([]RepoPresetRepository, 0, len(preset.Repos))
		seenRepos := make(map[string]struct{}, len(preset.Repos))
		for j, raw := range preset.Repos {
			providerPart := strings.TrimSpace(raw.Provider)
			if providerPart == "" || providerPart != strings.ToLower(providerPart) {
				return fmt.Errorf(
					"repo_presets[%d].repos[%d]: repository identity must use a canonical provider", i, j,
				)
			}
			provider, err := normalizePlatform(providerPart)
			if err != nil {
				return fmt.Errorf("repo_presets[%d].repos[%d]: %w", i, j, err)
			}
			if _, ok := platformpkg.MetadataFor(platformpkg.Kind(provider)); !ok {
				return fmt.Errorf(
					"repo_presets[%d].repos[%d]: unsupported provider %q", i, j, provider,
				)
			}
			host := strings.TrimSpace(raw.PlatformHost)
			repoPath := cleanPath(strings.TrimSpace(raw.RepoPath))
			platformRepoID := strings.TrimSpace(raw.PlatformRepoID)
			if platformRepoID == "" {
				return fmt.Errorf(
					"repo_presets[%d].repos[%d]: platform_repo_id is required", i, j,
				)
			}
			host, err = normalizePlatformHost(provider, host)
			if err != nil {
				return fmt.Errorf("repo_presets[%d].repos[%d]: %w", i, j, err)
			}
			repoPath = cleanPath(strings.TrimSpace(repoPath))
			if platformpkg.AllowsNestedOwner(platformpkg.Kind(provider)) {
				_, _, err = splitGitLabPath(repoPath, repoPath)
			} else {
				_, _, err = splitGitHubPath(repoPath, repoPath)
			}
			if err != nil {
				return fmt.Errorf(
					"repo_presets[%d].repos[%d]: repository identity must be provider|platform_host/repo_path", i, j,
				)
			}
			canonical := provider + "|" + host + "|" + platformRepoID
			if _, exists := seenRepos[canonical]; exists {
				continue
			}
			seenRepos[canonical] = struct{}{}
			repos = append(repos, RepoPresetRepository{
				Provider: provider, PlatformHost: host,
				PlatformRepoID: platformRepoID, RepoPath: repoPath,
			})
		}
		if len(repos) == 0 {
			return fmt.Errorf("repo_presets[%d]: at least one repository is required", i)
		}
		preset.Repos = repos
	}
	return nil
}

// DocFolder names a markdown folder registered for docs mode. Path
// normalization and existence checks are handled by the docs registry
// when the folder is used or edited.
type DocFolder struct {
	ID     string `toml:"id" json:"id"`
	Name   string `toml:"name" json:"name"`
	Path   string `toml:"path" json:"path"`
	Daemon string `toml:"daemon,omitempty" json:"daemon,omitempty"`
}

type PlatformConfig struct {
	Type          string `toml:"type" json:"type"`
	Host          string `toml:"host" json:"host"`
	BaseURL       string `toml:"base_url,omitempty" json:"base_url,omitempty"`
	AllowInsecure bool   `toml:"allow_insecure,omitempty" json:"allow_insecure,omitempty"`
	TokenEnv      string `toml:"token_env,omitempty" json:"token_env,omitempty"`
	TokenFile     string `toml:"token_file,omitempty" json:"token_file,omitempty"`
}

// GitHubOwnerTokenConfig maps one exact GitHub resource owner to a PAT
// source. The host defaults to github.com during validation.
type GitHubOwnerTokenConfig struct {
	Host      string `toml:"host,omitempty" json:"host,omitempty"`
	Owner     string `toml:"owner" json:"owner"`
	TokenEnv  string `toml:"token_env,omitempty" json:"token_env,omitempty"`
	TokenFile string `toml:"token_file,omitempty" json:"token_file,omitempty"`
}

// GitHubAppConfig registers a GitHub App created by the
// kenn-forge-github-app CLI. Installation tokens minted from the app's
// private key carry their own rate-limit budget, taking sync traffic
// off the host's PAT.
//
// Scope decision: one sync app and one archive app may serve each GitHub host
// and installation account. An installation token only reaches repos the
// installation covers. "All repositories" installations build owner routes;
// "Only select repositories" installations build exact routes for the recorded
// selected set, while uncovered repos fall through to the owner or host PAT
// chain. An entry without an installation_id is dormant and the PAT chain
// stays in effect.
type GitHubAppConfig struct {
	Host string `toml:"host" json:"host"`
	// Role selects which Forge workload uses this App. Empty values retain
	// the normal sync role for backwards-compatible configuration files.
	Role                string `toml:"role,omitempty" json:"role,omitempty"`
	AppID               int64  `toml:"app_id" json:"app_id"`
	Slug                string `toml:"slug,omitempty" json:"slug,omitempty"`
	Owner               string `toml:"owner,omitempty" json:"owner,omitempty"`
	OwnerType           string `toml:"owner_type,omitempty" json:"owner_type,omitempty"`
	PrivateKeyPath      string `toml:"private_key_path" json:"private_key_path"`
	InstallationID      int64  `toml:"installation_id,omitempty" json:"installation_id,omitempty"`
	InstallationAccount string `toml:"installation_account,omitempty" json:"installation_account,omitempty"`
	// RepositorySelection records whether the installation covers
	// "all" repositories of the account or only "selected" ones, as
	// reported by GitHub when the CLI recorded the installation.
	RepositorySelection string `toml:"repository_selection,omitempty" json:"repository_selection,omitempty"`
	// SelectedRepos lists the full names (owner/name) an "Only select
	// repositories" installation could reach when the CLI recorded it.
	// This is a startup routing snapshot: kenn-forge does not detect selection
	// changes made on GitHub afterwards. Narrowed access can surface as sync
	// 404s, while newly granted repositories keep using PAT fallback. Re-run
	// "kenn-forge-github-app install" and restart kenn-forge to load either
	// change into the bounded route table.
	SelectedRepos []string `toml:"selected_repos,omitempty" json:"selected_repos,omitempty"`
}

func (r Repo) FullName() string {
	return r.Owner + "/" + r.Name
}

func (r Repo) HasNameGlob() bool {
	return strings.ContainsAny(r.Name, "*?[")
}

// PlatformHostOrDefault returns the configured platform host,
// defaulting to the provider's public host when empty.
func (r Repo) PlatformHostOrDefault() string {
	if r.PlatformHost == "" {
		if host, ok := platformpkg.DefaultHost(platformpkg.Kind(r.PlatformOrDefault())); ok {
			return host
		}
		return defaultPlatformHost
	}
	return r.PlatformHost
}

func (r Repo) PlatformOrDefault() string {
	if r.Platform == "" {
		return defaultPlatform
	}
	return r.Platform
}

// ResolveToken returns the token for this repo. When TokenEnv is
// set, it reads from that env var. Falls back to globalToken if
// the env var is empty or TokenEnv is not set.
func (r Repo) ResolveToken(globalToken string) string {
	if r.TokenEnv != "" {
		if tok := os.Getenv(r.TokenEnv); tok != "" {
			return tok
		}
	}
	return globalToken
}

// normalize cleans up a Repo entry, extracting platform, host,
// owner, and name from provider URLs or SSH addresses if the user
// pasted one into either field. It also strips a trailing .git suffix.
func (r *Repo) normalize(defaultGitHubHost string) error {
	hadPlatformHost := strings.TrimSpace(r.PlatformHost) != ""
	platform, err := normalizePlatform(r.Platform)
	if err != nil {
		return err
	}
	r.Platform = platform

	// Check if either field contains a full GitHub URL or SSH
	// address. If so, extract owner/name from it.
	for _, raw := range []string{r.Owner, r.Name} {
		ref, err := parseRepoRef(raw, r.Platform)
		if err != nil {
			return err
		}
		if ref.owner != "" {
			r.Platform = ref.platform
			if !hadPlatformHost {
				r.PlatformHost = ref.host
				hadPlatformHost = true
			}
			r.Owner = ref.owner
			r.Name = ref.name
			r.RepoPath = ref.owner + "/" + ref.name
			break
		}
	}

	r.RepoPath = cleanPath(strings.TrimSpace(r.RepoPath))
	if r.RepoPath != "" && (strings.TrimSpace(r.Owner) == "" || strings.TrimSpace(r.Name) == "") {
		if platformpkg.AllowsNestedOwner(platformpkg.Kind(r.Platform)) {
			owner, name, err := splitGitLabPath("repo_path", r.RepoPath)
			if err != nil {
				return err
			}
			r.Owner = owner
			r.Name = name
		} else {
			owner, name, err := splitGitHubPath("repo_path", r.RepoPath)
			if err != nil {
				return err
			}
			r.Owner = owner
			r.Name = name
		}
	}
	r.Name = strings.TrimSuffix(r.Name, ".git")
	if r.Owner == "" || r.Name == "" {
		return errors.New("must have owner and name")
	}
	r.PlatformRepoID = strings.TrimSpace(r.PlatformRepoID)
	r.WorktreeBasePath = strings.TrimSpace(r.WorktreeBasePath)
	if r.WorktreeBasePath != "" && r.HasNameGlob() {
		return errors.New("worktree_base_path is only supported for exact repositories")
	}
	if platformpkg.LowercaseRepoNames(platformpkg.Kind(r.Platform)) {
		r.Owner = strings.ToLower(r.Owner)
		r.Name = strings.ToLower(r.Name)
		if r.RepoPath != "" {
			r.RepoPath = strings.ToLower(r.RepoPath)
		}
	}
	r.PlatformHost, err = normalizePlatformHost(r.Platform, r.PlatformHost)
	if err != nil {
		return err
	}
	if r.Platform == defaultPlatform && !hadPlatformHost {
		r.PlatformHost = defaultGitHubHost
	}
	if r.Platform == defaultPlatform &&
		r.PlatformHost == defaultPlatformHost &&
		defaultGitHubHost == defaultPlatformHost &&
		!hadPlatformHost {
		r.PlatformHost = ""
	}
	return nil
}

func (r Repo) ownerHasGlob() bool {
	return strings.ContainsAny(r.Owner, "*?[")
}

// nameHasGlob reports whether the entry names a pattern rather than one
// repository. A pattern's members are discovered at runtime, so its own
// credential route is a discovery aid rather than the credential that serves
// any particular repository.
func (r Repo) nameHasGlob() bool {
	return strings.ContainsAny(r.Name, "*?[")
}

type parsedRepoRef struct {
	platform string
	host     string
	owner    string
	name     string
}

func parseRepoRef(raw, configuredPlatform string) (parsedRepoRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return parsedRepoRef{}, nil
	}

	platform, err := normalizePlatform(configuredPlatform)
	if err != nil {
		return parsedRepoRef{}, err
	}

	var host, path string
	switch {
	case strings.HasPrefix(raw, "ssh://"):
		u, err := url.Parse(raw)
		if err != nil {
			return parsedRepoRef{}, fmt.Errorf("invalid SSH URI %q: %w", raw, err)
		}
		host = strings.ToLower(u.Hostname())
		path = strings.TrimPrefix(u.Path, "/")
	case strings.HasPrefix(raw, "http://") ||
		strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil {
			return parsedRepoRef{}, fmt.Errorf("invalid repository URL %q: %w", raw, err)
		}
		host = strings.ToLower(u.Host)
		path = strings.TrimPrefix(u.Path, "/")
	default:
		if m := scpRepoRe.FindStringSubmatch(raw); m != nil {
			host = strings.ToLower(m[1])
			path = m[2]
		} else if m := bareHostRepoRe.FindStringSubmatch(raw); m != nil {
			host = strings.ToLower(m[1])
			path = m[2]
		} else {
			return parsedRepoRef{}, nil
		}
	}

	if host == "" {
		return parsedRepoRef{}, nil
	}
	refPlatform, ok := platformForRepoRefHost(host, platform)
	if !ok {
		return parsedRepoRef{}, nil
	}

	path = cleanPath(path)
	if platformpkg.AllowsNestedOwner(platformpkg.Kind(refPlatform)) {
		owner, name, err := splitGitLabPath(raw, path)
		if err != nil {
			return parsedRepoRef{}, err
		}
		return parsedRepoRef{
			platform: refPlatform,
			host:     normalizePublicHost(host),
			owner:    owner,
			name:     name,
		}, nil
	}
	{
		owner, name, err := splitGitHubPath(raw, path)
		if err != nil {
			return parsedRepoRef{}, err
		}
		return parsedRepoRef{
			platform: refPlatform,
			host:     normalizePublicHost(host),
			owner:    owner,
			name:     name,
		}, nil
	}
}

func splitGitHubPath(raw, path string) (string, string, error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf(
			"incomplete GitHub reference %q: expected owner/repo", raw,
		)
	}
	return parts[0], parts[1], nil
}

func splitGitLabPath(raw, path string) (string, string, error) {
	parts := stripGitLabWebUISuffix(strings.Split(path, "/"))
	if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf(
			"incomplete GitLab reference %q: expected namespace/repo", raw,
		)
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
}

func stripGitLabWebUISuffix(parts []string) []string {
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] != "-" {
			continue
		}
		switch parts[i+1] {
		case "merge_requests", "issues", "tree", "blob", "commit", "commits":
			return parts[:i]
		}
	}
	return parts
}

func platformForRepoRefHost(host, configuredPlatform string) (string, bool) {
	host = normalizePublicHost(host)
	matchHost := hostNameForMatch(host)
	if configuredPlatform != defaultPlatform {
		return configuredPlatform, true
	}
	if matchHost == defaultPlatformHost {
		return defaultPlatform, true
	}
	if matchHost == platformpkg.DefaultForgejoHost {
		return string(platformpkg.KindForgejo), true
	}
	if matchHost == platformpkg.DefaultGiteaHost {
		return string(platformpkg.KindGitea), true
	}
	return "", false
}

func hostNameForMatch(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

func normalizePublicHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if before, ok := strings.CutSuffix(host, ":443"); ok {
		return before
	}
	return host
}

func normalizePlatform(raw string) (string, error) {
	kind, err := platformpkg.NormalizeKind(raw)
	if err != nil {
		return "", err
	}
	return string(kind), nil
}

// NormalizePlatformHost normalizes a configured provider host and rejects
// URL authority forms that could redirect provider tokens through userinfo or
// malformed host parsing.
func NormalizePlatformHost(platform, raw string) (string, error) {
	return normalizePlatformHost(platform, raw)
}

func normalizePlatformHost(platform, raw string) (string, error) {
	platform, err := normalizePlatform(platform)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		if defaultHost, ok := platformpkg.DefaultHost(platformpkg.Kind(platform)); ok {
			return defaultHost, nil
		}
		return "", fmt.Errorf("platform_host is required for platform %q", platform)
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		u, err := url.Parse(host)
		if err != nil {
			return "", fmt.Errorf("invalid_repo_ref: invalid platform_host %q: %w", raw, err)
		}
		if u.User != nil {
			return "", fmt.Errorf("invalid_repo_ref: platform_host %q must not include userinfo", raw)
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf(
				"invalid_repo_ref: platform_host %q must be a host; subpath installs are not supported",
				raw,
			)
		}
		host = u.Host
	} else {
		host = strings.TrimRight(host, "/")
		if strings.Contains(host, "/") {
			return "", fmt.Errorf(
				"invalid_repo_ref: platform_host %q must be a host; subpath installs are not supported",
				raw,
			)
		}
	}
	return normalizePlatformHostAuthority(raw, host)
}

func normalizePlatformHostAuthority(raw, host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", fmt.Errorf("invalid_repo_ref: platform_host %q is empty", raw)
	}
	if strings.Contains(host, "@") {
		return "", fmt.Errorf("invalid_repo_ref: platform_host %q must not include userinfo", raw)
	}
	if strings.ContainsFunc(host, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) {
		return "", fmt.Errorf("invalid_repo_ref: platform_host %q contains invalid characters", raw)
	}
	if err := validatePlatformHostPort(raw, host); err != nil {
		return "", err
	}

	parsed, err := url.Parse("//" + host)
	if err != nil {
		return "", fmt.Errorf("invalid_repo_ref: invalid platform_host %q: %w", raw, err)
	}
	if parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "" {
		return "", fmt.Errorf("invalid_repo_ref: platform_host %q must be a host", raw)
	}
	return normalizePublicHost(host), nil
}

func validatePlatformHostPort(raw, host string) error {
	if strings.HasPrefix(host, "[") {
		closing := strings.LastIndex(host, "]")
		if closing == -1 {
			return fmt.Errorf("invalid_repo_ref: invalid platform_host %q", raw)
		}
		if closing == len(host)-1 {
			return nil
		}
		if host[closing+1] != ':' {
			return fmt.Errorf("invalid_repo_ref: invalid platform_host %q", raw)
		}
		return validatePlatformHostPortNumber(raw, host[closing+2:])
	}

	colonCount := strings.Count(host, ":")
	switch colonCount {
	case 0:
		return nil
	case 1:
		_, port, _ := strings.Cut(host, ":")
		return validatePlatformHostPortNumber(raw, port)
	default:
		return fmt.Errorf("invalid_repo_ref: platform_host %q must bracket IPv6 literals", raw)
	}
}

func validatePlatformHostPortNumber(raw, port string) error {
	if port == "" {
		return fmt.Errorf("invalid_repo_ref: platform_host %q has an empty port", raw)
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid_repo_ref: platform_host %q has a non-numeric port", raw)
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber > 65535 {
		return fmt.Errorf("invalid_repo_ref: platform_host %q has an invalid port", raw)
	}
	return nil
}

// cleanPath strips query strings, fragments, trailing slashes,
// and an optional .git suffix from a GitHub ref path.
func cleanPath(path string) string {
	if idx := strings.IndexAny(path, "?#"); idx != -1 {
		path = path[:idx]
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	return path
}

type Activity struct {
	ViewMode                       string `toml:"view_mode" json:"view_mode" enum:"flat,threaded"`
	TimeRange                      string `toml:"time_range" json:"time_range" enum:"24h,7d,30d,90d"`
	HideClosed                     bool   `toml:"hide_closed" json:"hide_closed"`
	HideBots                       bool   `toml:"hide_bots" json:"hide_bots"`
	CollapseThreads                bool   `toml:"collapse_threads" json:"collapse_threads"`
	UseWorkspaceActivityForRecency bool   `toml:"use_workspace_activity_for_recency" json:"use_workspace_activity_for_recency"`
	DefaultBranchRetentionDays     int    `toml:"default_branch_retention_days" json:"default_branch_retention_days"`
	DefaultBranchMaxCommits        int    `toml:"default_branch_max_commits" json:"default_branch_max_commits"`
}

// PullRequests configures safeguards around pull-request mutations.
type PullRequests struct {
	// AllowMidStackMerges permits merging a stack member while an earlier
	// member remains unmerged. The default is deliberately false.
	AllowMidStackMerges bool `toml:"allow_mid_stack_merges,omitempty" json:"allow_mid_stack_merges"`
	// PreferGitHubNativeStacks opts into GitHub's read-only stack preview.
	// Branch-based detection remains the fallback when native data is unusable.
	PreferGitHubNativeStacks bool `toml:"prefer_github_native_stacks,omitempty" json:"prefer_github_native_stacks"`
}

// Detail configures presentation shared by pull-request and issue detail.
type Detail struct {
	InitialTimelineEntryLimit int `toml:"initial_timeline_entry_limit,omitempty" json:"initial_timeline_entry_limit" minimum:"10" maximum:"250"`
	// CollapseSingleLineBreaks renders markdown with CommonMark soft breaks:
	// a single newline joins the paragraph and only a blank line separates
	// paragraphs. False keeps GitHub comment-style hard breaks.
	CollapseSingleLineBreaks bool `toml:"collapse_single_line_breaks,omitempty" json:"collapse_single_line_breaks"`
	// RenderCommitMessagesAsMarkdown renders commit bodies in the timeline
	// through the same markdown pipeline as comments instead of plain text.
	RenderCommitMessagesAsMarkdown bool `toml:"render_commit_messages_as_markdown,omitempty" json:"render_commit_messages_as_markdown"`
}

// Workspaces configures behavior shared by PR- and issue-backed workspaces.
type Workspaces struct {
	// AutoAssignOnCreate adds the authenticated provider user to the source
	// PR or issue when kenn-forge creates its workspace.
	AutoAssignOnCreate bool `toml:"auto_assign_on_create,omitempty" json:"auto_assign_on_create"`
	// DefaultSidebarView selects the initial right-sidebar tab for a workspace
	// created from a pull request or issue. A saved per-workspace choice wins.
	DefaultSidebarView string `toml:"default_sidebar_view,omitempty" json:"default_sidebar_view" enum:"diff,item"`
}

func (w Workspaces) withDefaults() Workspaces {
	if w.DefaultSidebarView == "" {
		w.DefaultSidebarView = "diff"
	}
	return w
}

// Issues configures issue-list presentation preferences.
type Issues struct {
	HideBots bool `toml:"hide_bots,omitempty" json:"hide_bots"`
}

const (
	DefaultTerminalFontSize         = 12
	DefaultTerminalScrollback       = 1000
	DefaultTerminalLineHeight       = 1.0
	DefaultTerminalCursorBlink      = true
	DefaultTerminalGraphics         = true
	DefaultTerminalTmuxMouse        = true
	DefaultTerminalRetainedSessions = 10
)

type Terminal struct {
	FontFamily       string  `toml:"font_family,omitempty" json:"font_family"`
	FontSize         int     `toml:"font_size,omitempty" json:"font_size"`
	Scrollback       int     `toml:"scrollback,omitempty" json:"scrollback"`
	LineHeight       float64 `toml:"line_height,omitempty" json:"line_height"`
	LetterSpacing    int     `toml:"letter_spacing,omitempty" json:"letter_spacing"`
	CursorBlink      *bool   `toml:"cursor_blink,omitempty" json:"cursor_blink" nullable:"false"`
	FontLigatures    bool    `toml:"font_ligatures,omitempty" json:"font_ligatures"`
	HideTmuxStatus   bool    `toml:"hide_tmux_status,omitempty" json:"hide_tmux_status"`
	Graphics         *bool   `toml:"graphics,omitempty" json:"graphics" nullable:"false"`
	TmuxMouse        *bool   `toml:"tmux_mouse,omitempty" json:"tmux_mouse" nullable:"false"`
	RetainedSessions *int    `toml:"retained_sessions,omitempty" json:"retained_sessions" nullable:"false"`
}

type Agent struct {
	Key     string   `toml:"key" json:"key"`
	Label   string   `toml:"label,omitempty" json:"label"`
	Command []string `toml:"command,omitempty" json:"command,omitempty" nullable:"false"`
	Enabled *bool    `toml:"enabled,omitempty" json:"enabled,omitempty"`
}

func (a Agent) EnabledOrDefault() bool {
	return a.Enabled == nil || *a.Enabled
}

type Roborev struct {
	Endpoint          string `toml:"endpoint,omitempty"`
	InitManagedClones bool   `toml:"init_managed_clones,omitempty"`
}

// ModeVisibility controls which top-level app modes are shown. Nil booleans
// mean the mode uses its default visibility.
type ModeVisibility struct {
	Activity   *bool `toml:"activity,omitempty" json:"activity" nullable:"false"`
	Repos      *bool `toml:"repos,omitempty" json:"repos" nullable:"false"`
	Docs       *bool `toml:"docs,omitempty" json:"docs" nullable:"false"`
	Actions    *bool `toml:"actions,omitempty" json:"actions" nullable:"false"`
	Pulls      *bool `toml:"pulls,omitempty" json:"pulls" nullable:"false"`
	Issues     *bool `toml:"issues,omitempty" json:"issues" nullable:"false"`
	Reviews    *bool `toml:"reviews,omitempty" json:"reviews" nullable:"false"`
	Workspaces *bool `toml:"workspaces,omitempty" json:"workspaces" nullable:"false"`
}

func DefaultModeVisibility() ModeVisibility {
	return ModeVisibility{
		Activity:   new(true),
		Repos:      new(true),
		Docs:       new(false),
		Actions:    new(false),
		Pulls:      new(true),
		Issues:     new(true),
		Reviews:    new(true),
		Workspaces: new(true),
	}
}

func (m ModeVisibility) WithDefaults() ModeVisibility {
	defaults := DefaultModeVisibility()
	if m.Activity != nil {
		defaults.Activity = m.Activity
	}
	if m.Repos != nil {
		defaults.Repos = m.Repos
	}
	if m.Docs != nil {
		defaults.Docs = m.Docs
	}
	if m.Actions != nil {
		defaults.Actions = m.Actions
	}
	if m.Pulls != nil {
		defaults.Pulls = m.Pulls
	}
	if m.Issues != nil {
		defaults.Issues = m.Issues
	}
	if m.Reviews != nil {
		defaults.Reviews = m.Reviews
	}
	if m.Workspaces != nil {
		defaults.Workspaces = m.Workspaces
	}
	return defaults
}

type Tmux struct {
	Command       []string `toml:"command,omitempty"`
	AgentSessions *bool    `toml:"agent_sessions,omitempty"`
}

type FleetSessions struct {
	IncludeUnmanagedDetails bool `toml:"include_unmanaged_details,omitempty" json:"include_unmanaged_details,omitempty"`
}

type FleetRole string

const (
	FleetRoleHub   FleetRole = "hub"
	FleetRoleSpoke FleetRole = "spoke"
)

type FleetHub struct {
	NodeID  string `toml:"node_id" json:"node_id"`
	Name    string `toml:"name,omitempty" json:"name,omitempty"`
	BaseURL string `toml:"base_url" json:"base_url"`
}

type FleetMember struct {
	NodeID  string                     `toml:"node_id" json:"node_id"`
	Name    string                     `toml:"name,omitempty" json:"name,omitempty"`
	BaseURL string                     `toml:"base_url" json:"base_url"`
	State   federation.EnrollmentState `toml:"state" json:"state"`
}

// Fleet configures this daemon's role and enrolled HTTP membership.
type Fleet struct {
	Enabled     bool          `toml:"enabled,omitempty" json:"enabled"`
	Role        FleetRole     `toml:"role,omitempty" json:"role,omitempty"`
	BaseURL     string        `toml:"base_url,omitempty" json:"base_url,omitempty"`
	Hub         *FleetHub     `toml:"hub,omitempty" json:"hub,omitempty"`
	Members     []FleetMember `toml:"members,omitempty" json:"members,omitempty"`
	PeerTimeout string        `toml:"peer_timeout,omitempty" json:"peer_timeout,omitempty"`
	Sessions    FleetSessions `toml:"sessions" json:"sessions"`
}

func (f Fleet) RoleOrDefault() FleetRole {
	role := FleetRole(strings.TrimSpace(string(f.Role)))
	if role == "" {
		return FleetRoleHub
	}
	return role
}

// PeerTimeoutOrDefault returns the per-peer fetch timeout, defaulting to
// 2s when unset or unparseable.
func (f Fleet) PeerTimeoutOrDefault() time.Duration {
	if f.PeerTimeout == "" {
		return 2 * time.Second
	}
	if d, err := time.ParseDuration(f.PeerTimeout); err == nil {
		return d
	}
	return 2 * time.Second
}

type Notifications struct {
	SyncInterval        string `toml:"sync_interval" json:"sync_interval"`
	PropagationInterval string `toml:"propagation_interval" json:"propagation_interval"`
	BatchSize           int    `toml:"batch_size" json:"batch_size"`
}

// Shell configures the command kenn-forge runs when ensuring the
// per-workspace plain shell session. Hardened kenn-forge deployments
// (e.g. systemd services with SystemCallFilter=~@privileged) must
// wrap the shell so it escapes the parent's seccomp filter — zsh
// calls setresuid during startup and is killed by SIGSYS otherwise.
// The configured command is invoked with the workspace worktree as
// its working directory; provide a command that propagates that to
// the spawned shell (e.g. `systemd-run --working-directory=...`).
type Shell struct {
	Command []string `toml:"command,omitempty"`
}

type MCP struct {
	Enabled     bool `toml:"enabled,omitempty" json:"enabled,omitempty"`
	Port        int  `toml:"port,omitempty" json:"port,omitempty"`
	DiffCacheMB int  `toml:"diff_cache_mb,omitempty" json:"diff_cache_mb,omitempty"`
}

type Config struct {
	SyncInterval              string `toml:"sync_interval"`
	ActivePRRefreshInterval   string `toml:"active_pr_refresh_interval"`
	ActivePRWindow            string `toml:"active_pr_window"`
	GitHubTokenEnv            string `toml:"github_token_env"`
	DefaultPlatformHost       string `toml:"default_platform_host"`
	Host                      string `toml:"host"`
	Port                      int    `toml:"port"`
	BasePath                  string `toml:"base_path"`
	DataDir                   string `toml:"data_dir"`
	SyncBudgetPerHour         int    `toml:"sync_budget_per_hour"`
	SSEBufferSize             int    `toml:"sse_buffer_size"`
	IssueWorkspaceBranchStyle string `toml:"issue_workspace_branch_style"`
	// AllowedHosts is an exact-match allowlist of Host header values
	// beyond the bind address that the Host validation middleware
	// should accept. Loopback synonyms (127.0.0.1 / localhost /
	// [::1]) at the bind port are auto-accepted and do not need to
	// be listed.
	AllowedHosts []string `toml:"allowed_hosts"`
	// TrustReverseProxy enables honoring X-Forwarded-Host and
	// Forwarded RFC 7239 host= for the Public Host validation step.
	// The raw Host header must still pass the allowed_hosts gate
	// before any forwarded header is read.
	TrustReverseProxy bool                     `toml:"trust_reverse_proxy"`
	Repos             []Repo                   `toml:"repos"`
	RepoPresets       []RepoPreset             `toml:"repo_presets"`
	KataProjects      []KataProjectRepoMapping `toml:"kata_projects"`
	Platforms         []PlatformConfig         `toml:"platforms"`
	GitHubOwnerTokens []GitHubOwnerTokenConfig `toml:"github_owner_tokens"`
	GitHubApps        []GitHubAppConfig        `toml:"github_apps"`
	Activity          Activity                 `toml:"activity"`
	PullRequests      PullRequests             `toml:"pull_requests"`
	Detail            Detail                   `toml:"detail"`
	Workspaces        Workspaces               `toml:"workspaces"`
	Issues            Issues                   `toml:"issues"`
	Notifications     Notifications            `toml:"notifications"`
	Terminal          Terminal                 `toml:"terminal"`
	Modes             ModeVisibility           `toml:"modes"`
	Agents            []Agent                  `toml:"agents"`
	DocFolders        []DocFolder              `toml:"doc_folders"`
	Roborev           Roborev                  `toml:"roborev"`
	Tmux              Tmux                     `toml:"tmux"`
	Shell             Shell                    `toml:"shell"`
	Fleet             Fleet                    `toml:"fleet"`
	API               API                      `toml:"api"`
	MCP               MCP                      `toml:"mcp"`

	// parsedAllowedHosts is the canonicalised form of AllowedHosts,
	// populated by Validate so the server constructor does not have
	// to re-parse on every request setup. Defensive copy via
	// ParsedAllowedHosts.
	parsedAllowedHosts []HostKey
	// parsedBindKey is the canonical (Host, Port) key for the bind
	// address, populated by Validate.
	parsedBindKey      HostKey
	dataDirWasRelative bool
}

// API configures the HTTP API surface.
type API struct {
	// RequireAuth enforces bearer-token auth on /api routes. The
	// token is minted under data_dir (auth_token, 0600) at serve
	// start; browsers bootstrap a session cookie via the tokenized
	// URL recorded in the runtime metadata. Health probes stay open.
	RequireAuth bool `toml:"require_auth,omitempty" json:"require_auth,omitempty"`
	// TailscaleServe authorizes selected Tailscale Serve users at the local
	// loopback request boundary. It does not alter federation credentials.
	TailscaleServe TailscaleServeAPI `toml:"tailscale_serve,omitempty" json:"tailscale_serve,omitzero"`
}

// TailscaleServeAPI configures the optional Tailscale Serve user principal.
type TailscaleServeAPI struct {
	Enabled      bool     `toml:"enabled,omitempty" json:"enabled,omitempty"`
	AllowedUsers []string `toml:"allowed_users,omitempty" json:"allowed_users,omitempty"`
}

// SSEBufferSizeOrDefault returns the configured SSE replay ring size,
// falling back to the package default. A nil receiver is treated as
// fully default-configured so tests that pass cfg = nil into the
// server still get a working ring size.
func (c *Config) SSEBufferSizeOrDefault() int {
	if c == nil || c.SSEBufferSize == 0 {
		return defaultSSEBufferSize
	}
	return c.SSEBufferSize
}

func (c *Config) MCPPort() int {
	if c == nil || !c.MCP.Enabled {
		return 0
	}
	if c.MCP.Port != 0 {
		return c.MCP.Port
	}
	return c.Port + 1
}

func (c *Config) MCPListenAddr() string {
	port := c.MCPPort()
	if port == 0 {
		return ""
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c *Config) MCPDiffCacheBytes() int64 {
	megabytes := defaultMCPDiffCacheMB
	if c != nil && c.MCP.DiffCacheMB != 0 {
		megabytes = c.MCP.DiffCacheMB
	}
	return int64(megabytes) << 20
}

// IsLoopbackHostname reports whether a URL hostname (no port, no
// brackets) is localhost or a loopback IP literal. Hostnames that
// merely resolve to loopback are not recognized.
func IsLoopbackHostname(hostname string) bool {
	if strings.EqualFold(strings.TrimSuffix(hostname, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func DefaultConfigPath() string {
	return filepath.Join(baseDir(), "config.toml")
}

func DefaultDataDir() string {
	return baseDir()
}

func baseDir() string {
	if d := os.Getenv("KENN_FORGE_HOME"); d != "" {
		return d
	}
	return filepath.Join(homeDir(), ".kenn", "forge")
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func defaultConfigContents() string {
	const defaultConfig = `# kenn-forge configuration
# See https://github.com/wesm/kenn-forge for documentation.

sync_interval = "5m"
github_token_env = "KENN_FORGE_GITHUB_TOKEN"
default_platform_host = "github.com"
host = "127.0.0.1"
port = 8091

# Per-member request timeout for an enrolled federation (default "2s").
# [fleet]
# enabled = false
# role = "hub"
# peer_timeout = "2s"

# Gate the HTTP API behind a bearer token (minted to
# <data_dir>/auth_token; browsers bootstrap a session cookie via the
# tokenized URL from the runtime metadata). Recommended when other
# local users share the machine.
# [api]
# require_auth = true

# Add additional provider hosts when needed.
# [[platforms]]
# type = "gitlab"
# host = "gitlab.com"
# token_env = "KENN_FORGE_GITLAB_TOKEN"

# Add repositories to monitor (or add them in the Settings UI).
# [[repos]]
# owner = "your-org"
# name = "your-repo"

[activity]
view_mode = "threaded"
time_range = "7d"
collapse_threads = true
default_branch_retention_days = 90
default_branch_max_commits = 5000

[detail]
initial_timeline_entry_limit = 50
collapse_single_line_breaks = false
render_commit_messages_as_markdown = false

[terminal]

[modes]
activity = true
repos = true
docs = false
pulls = true
issues = true
reviews = true
workspaces = true

[notifications]
sync_interval = "2m"
propagation_interval = "1m"
batch_size = 25

[tmux]
agent_sessions = true
`
	return defaultConfig
}

// EnsureDefault creates a default config file at path if it does not exist.
// The file contains sensible defaults. Repos can be added later through the
// settings UI.
//
// Writes to a temp file first, then hard-links into place so the target
// path is never left empty or partially written.
func EnsureDefault(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(defaultConfigContents()); err != nil {
		tmp.Close()
		return fmt.Errorf("writing default config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flushing default config: %w", err)
	}

	// Link fails atomically when path already exists, providing
	// both atomic install and race-free existence check.
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		// Hard links may not be supported (FAT/exFAT, network
		// shares, cross-device). Fall back to O_EXCL create +
		// write with cleanup on failure.
		return writeExclusive(tmpPath, path)
	}
	return nil
}

// writeExclusive creates dst with O_EXCL (fails if it exists) and
// copies the content from src. Partial files are removed on failure.
func writeExclusive(src, dst string) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading temp config: %w", err)
	}

	f, err := os.OpenFile(
		dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("creating config %s: %w", dst, err)
	}

	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(dst)
		return fmt.Errorf("writing config %s: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("flushing config %s: %w", dst, err)
	}
	return nil
}

func Load(path string) (*Config, error) {
	cfg, err := load(path)
	if err != nil {
		// cfg is non-nil for post-decode rejections (deprecated keys);
		// callers treat any error as failure but reload-side stripping
		// may still read declared token names from the candidate.
		return cfg, err
	}
	return cfg, cfg.validate()
}

// load reads and normalizes path without running validation.
func load(path string) (*Config, error) {
	cfg := &Config{
		SyncInterval:            defaultSyncInterval,
		ActivePRRefreshInterval: defaultActivePRRefreshInterval,
		ActivePRWindow:          defaultActivePRWindow,
		GitHubTokenEnv:          defaultGitHubTokenEnv,
		DefaultPlatformHost:     defaultPlatformHost,
		Host:                    defaultHost,
		Port:                    defaultPort,
		Activity: Activity{
			CollapseThreads: true,
		},
		Detail: Detail{
			InitialTimelineEntryLimit: DefaultInitialTimelineEntryLimit,
		},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	meta, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := rejectUnsupportedConfigKeys(meta); err != nil {
		// The decode succeeded, so return the populated config with the
		// rejection: a reload must still see a rejected candidate's
		// declared token env names to keep stripping them.
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Repos == nil {
		cfg.Repos = []Repo{}
	}
	if cfg.Platforms == nil {
		cfg.Platforms = []PlatformConfig{}
	}
	if cfg.Agents == nil {
		cfg.Agents = []Agent{}
	}
	cfg.Modes = cfg.Modes.WithDefaults()
	cfg.Workspaces = cfg.Workspaces.withDefaults()

	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	cfg.dataDirWasRelative = !filepath.IsAbs(cfg.DataDir)
	canonicalDir, err := CanonicalDataDir(cfg.DataDir)
	if err != nil {
		// The decode succeeded; return the config with the error so a
		// rejected reload can still see its declared token names.
		return cfg, err
	}
	cfg.DataDir = canonicalDir

	if cfg.Activity.ViewMode == "" {
		cfg.Activity.ViewMode = defaultViewMode
	}
	if cfg.Activity.TimeRange == "" {
		cfg.Activity.TimeRange = defaultTimeRange
	}
	if cfg.Activity.DefaultBranchRetentionDays == 0 {
		cfg.Activity.DefaultBranchRetentionDays = defaultBranchActivityRetentionDays
	}
	if cfg.Activity.DefaultBranchMaxCommits == 0 {
		cfg.Activity.DefaultBranchMaxCommits = defaultBranchActivityMaxCommits
	}
	if cfg.Detail.InitialTimelineEntryLimit == 0 {
		cfg.Detail.InitialTimelineEntryLimit = DefaultInitialTimelineEntryLimit
	}
	if cfg.Notifications.SyncInterval == "" {
		cfg.Notifications.SyncInterval = defaultNotificationSyncInterval
	}
	if cfg.Notifications.PropagationInterval == "" {
		cfg.Notifications.PropagationInterval = defaultNotificationPropagationInterval
	}
	if cfg.Notifications.BatchSize == 0 {
		cfg.Notifications.BatchSize = 25
	}

	if cfg.SyncBudgetPerHour == 0 {
		cfg.SyncBudgetPerHour = defaultSyncBudgetPerHour
	}

	if strings.TrimSpace(cfg.IssueWorkspaceBranchStyle) == "" {
		cfg.IssueWorkspaceBranchStyle = defaultIssueWorkspaceBranchStyle
	}

	if cfg.SSEBufferSize == 0 {
		cfg.SSEBufferSize = defaultSSEBufferSize
	}

	if cfg.BasePath == "" {
		cfg.BasePath = defaultBasePath
	} else {
		bp := "/" + strings.Trim(cfg.BasePath, "/")
		if bp != "/" {
			bp += "/"
		}
		cfg.BasePath = bp
	}

	cfg.normalizeTokenFilePaths(filepath.Dir(path))
	return cfg, nil
}

// LoadForGitHubAppRepair loads path for GitHub App management commands while
// retaining the ordinary structural validation rules.
func LoadForGitHubAppRepair(path string) (*Config, error) {
	cfg, err := load(path)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.validate()
}

func rejectUnsupportedConfigKeys(meta toml.MetaData) error {
	for _, key := range meta.Undecoded() {
		if len(key) >= 1 && (key[0] == "notebooks" || key[0] == "vaults") {
			return fmt.Errorf("[[%s]] is not supported; use [[doc_folders]]", key[0])
		}
		if len(key) >= 2 && key[0] == "fleet" {
			return fmt.Errorf(
				"unsupported fleet configuration %q; enroll federation members instead",
				key.String(),
			)
		}
	}
	return nil
}

func (c *Config) Validate() error {
	return c.validate()
}

// DataDirWasRelative reports whether data_dir was relative before loading
// established its canonical runtime identity.
func (c *Config) DataDirWasRelative() bool {
	return c.dataDirWasRelative
}

// validate runs every config rule.
func (c *Config) validate() error {
	var err error
	if err := c.Fleet.Validate(); err != nil {
		return err
	}
	if err := c.API.TailscaleServe.Validate(); err != nil {
		return err
	}
	if c.API.TailscaleServe.Enabled && !c.API.RequireAuth {
		return errors.New("config: api.tailscale_serve.enabled requires api.require_auth = true")
	}
	if c.Fleet.Enabled && !c.API.RequireAuth {
		return errors.New("config: fleet.enabled requires api.require_auth = true")
	}
	if c.Fleet.Enabled && c.BasePath != "" && c.BasePath != defaultBasePath {
		return errors.New("config: fleet.enabled requires base_path = \"/\"")
	}
	c.DefaultPlatformHost, err = normalizePlatformHost(
		defaultPlatform, c.DefaultPlatformHost,
	)
	if err != nil {
		return fmt.Errorf("config: default_platform_host: %w", err)
	}
	if c.DefaultPlatformHost == defaultPlatformHost {
		c.DefaultPlatformHost = defaultPlatformHost
	}

	for i := range c.Platforms {
		p := &c.Platforms[i]
		p.Type, err = normalizePlatform(p.Type)
		if err != nil {
			return fmt.Errorf("config: platforms[%d]: %w", i, err)
		}
		p.Host, err = normalizePlatformHost(p.Type, p.Host)
		if err != nil {
			return fmt.Errorf("config: platforms[%d]: %w", i, err)
		}
		p.TokenEnv = strings.TrimSpace(p.TokenEnv)
		p.TokenFile = strings.TrimSpace(p.TokenFile)
		if err := normalizePlatformTransport(p); err != nil {
			return fmt.Errorf("config: platforms[%d]: %w", i, err)
		}
	}
	if err := c.validatePlatforms(); err != nil {
		return err
	}
	if err := c.validateGitHubOwnerTokens(); err != nil {
		return err
	}
	if err := c.validateTokenEnvNamesNotTerminalVars(); err != nil {
		return err
	}
	if err := c.validateGitHubApps(); err != nil {
		return err
	}
	if err := c.canonicalizeDocFolders(); err != nil {
		return err
	}
	c.Modes = c.Modes.WithDefaults()
	c.Workspaces = c.Workspaces.withDefaults()
	if c.Workspaces.DefaultSidebarView != "diff" && c.Workspaces.DefaultSidebarView != "item" {
		return fmt.Errorf(
			"config: workspaces.default_sidebar_view must be one of diff or item",
		)
	}

	for i := range c.Repos {
		if c.Repos[i].ownerHasGlob() {
			return fmt.Errorf(
				"config: repos[%d]: glob syntax in owner is not supported", i,
			)
		}
		if err := c.Repos[i].normalize(c.DefaultPlatformHost); err != nil {
			return fmt.Errorf("config: repos[%d]: %w", i, err)
		}
		c.Repos[i].TokenEnv = strings.TrimSpace(c.Repos[i].TokenEnv)
		c.Repos[i].TokenFile = strings.TrimSpace(c.Repos[i].TokenFile)
		if c.Repos[i].PlatformOrDefault() == defaultPlatform &&
			c.Repos[i].HasNameGlob() &&
			(c.Repos[i].TokenEnv != "" || c.Repos[i].TokenFile != "") {
			return fmt.Errorf(
				"config: repos[%d]: GitHub repo token override requires an exact repository name", i,
			)
		}
	}

	// Reject duplicate repository identities.
	seen := make(map[string]string, len(c.Repos))
	for _, r := range c.Repos {
		key := repoIdentityKey(r)
		display := repoIdentityDisplay(r)
		if prev, ok := seen[key]; ok {
			return fmt.Errorf(
				"config: duplicate repo %q", prev,
			)
		}
		seen[key] = display
	}
	if err := c.validateKataProjectRepoMappings(); err != nil {
		return err
	}
	if err := normalizeRepoPresets(c.RepoPresets); err != nil {
		return err
	}

	if err := c.ValidateRepoTokenSourceConsistency(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	if _, err := time.ParseDuration(c.SyncInterval); err != nil {
		return fmt.Errorf("config: invalid sync_interval %q: %w", c.SyncInterval, err)
	}
	if c.ActivePRRefreshInterval == "" {
		c.ActivePRRefreshInterval = defaultActivePRRefreshInterval
	}
	if c.ActivePRWindow == "" {
		c.ActivePRWindow = defaultActivePRWindow
	}
	if d, err := time.ParseDuration(c.ActivePRRefreshInterval); err != nil {
		return fmt.Errorf("config: invalid active_pr_refresh_interval %q: %w", c.ActivePRRefreshInterval, err)
	} else if d <= 0 {
		return fmt.Errorf("config: active_pr_refresh_interval must be positive, got %q", c.ActivePRRefreshInterval)
	}
	if d, err := time.ParseDuration(c.ActivePRWindow); err != nil {
		return fmt.Errorf("config: invalid active_pr_window %q: %w", c.ActivePRWindow, err)
	} else if d <= 0 {
		return fmt.Errorf("config: active_pr_window must be positive, got %q", c.ActivePRWindow)
	}
	if c.Notifications.SyncInterval == "" {
		c.Notifications.SyncInterval = defaultNotificationSyncInterval
	}
	if c.Notifications.PropagationInterval == "" {
		c.Notifications.PropagationInterval = defaultNotificationPropagationInterval
	}
	if c.Notifications.BatchSize == 0 {
		c.Notifications.BatchSize = 25
	}
	if d, err := time.ParseDuration(c.Notifications.SyncInterval); err != nil {
		return fmt.Errorf("config: invalid notifications.sync_interval %q: %w", c.Notifications.SyncInterval, err)
	} else if d <= 0 {
		return fmt.Errorf("config: notifications.sync_interval must be positive, got %q", c.Notifications.SyncInterval)
	}
	if d, err := time.ParseDuration(c.Notifications.PropagationInterval); err != nil {
		return fmt.Errorf("config: invalid notifications.propagation_interval %q: %w", c.Notifications.PropagationInterval, err)
	} else if d <= 0 {
		return fmt.Errorf("config: notifications.propagation_interval must be positive, got %q", c.Notifications.PropagationInterval)
	}
	if c.Notifications.BatchSize < 1 || c.Notifications.BatchSize > 200 {
		return fmt.Errorf("config: notifications.batch_size must be between 1 and 200, got %d", c.Notifications.BatchSize)
	}

	if ip := net.ParseIP(c.Host); ip == nil {
		return fmt.Errorf("config: invalid host %q (must be an IP address)", c.Host)
	} else if ip.IsUnspecified() {
		return fmt.Errorf(
			"config: host %q is unspecified; bind a specific address"+
				" (loopback, or one interface such as a tailnet IP) so"+
				" the unauthenticated API is only exposed on a network"+
				" you trust", c.Host,
		)
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("config: invalid port %d", c.Port)
	}
	if c.MCP.Port != 0 && (c.MCP.Port < 1 || c.MCP.Port > 65535) {
		return fmt.Errorf("config: invalid MCP port %d", c.MCP.Port)
	}
	if c.MCP.DiffCacheMB < 0 || c.MCP.DiffCacheMB > math.MaxInt64>>20 {
		return fmt.Errorf("config: MCP diff cache size is outside the supported range")
	}
	if c.MCP.Enabled {
		mcpPort := c.MCPPort()
		if mcpPort < 1 || mcpPort > 65535 {
			return fmt.Errorf("config: invalid resolved MCP port %d", mcpPort)
		}
		if mcpPort == c.Port {
			return fmt.Errorf("config: MCP port %d matches backend port", mcpPort)
		}
		if !IsLoopbackHostname(c.Host) {
			return fmt.Errorf("config: MCP listener requires a loopback host, got %q", c.Host)
		}
	}

	bindKey, err := ParseHostKey(net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		return fmt.Errorf("config: invalid host %q: %w", c.Host, err)
	}
	c.parsedBindKey = bindKey

	c.parsedAllowedHosts = c.parsedAllowedHosts[:0]
	for _, entry := range c.AllowedHosts {
		key, err := ParseHostKey(entry)
		if err != nil {
			return fmt.Errorf("config: invalid allowed_hosts entry %q: %w", entry, err)
		}
		c.parsedAllowedHosts = append(c.parsedAllowedHosts, key)
	}

	if c.SyncBudgetPerHour != 0 && c.SyncBudgetPerHour < 50 {
		return fmt.Errorf(
			"config: sync_budget_per_hour must be >= 50 or omitted, got %d",
			c.SyncBudgetPerHour,
		)
	}

	if c.SSEBufferSize != 0 &&
		(c.SSEBufferSize < minSSEBufferSize || c.SSEBufferSize > maxSSEBufferSize) {
		return fmt.Errorf(
			"config: sse_buffer_size must be between %d and %d or omitted, got %d",
			minSSEBufferSize, maxSSEBufferSize, c.SSEBufferSize,
		)
	}

	if !validBasePathRe.MatchString(c.BasePath) {
		return fmt.Errorf("config: invalid base_path %q: must be / or /path/ using only alphanumerics, hyphens, underscores, dots, and tildes", c.BasePath)
	}
	for seg := range strings.SplitSeq(strings.Trim(c.BasePath, "/"), "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("config: invalid base_path %q: dot segments are not allowed", c.BasePath)
		}
	}

	validViewModes := map[string]bool{
		"flat": true, "threaded": true,
	}
	if !validViewModes[c.Activity.ViewMode] {
		return fmt.Errorf(
			"config: invalid activity view_mode %q",
			c.Activity.ViewMode,
		)
	}
	validTimeRanges := map[string]bool{
		"24h": true, "7d": true, "30d": true, "90d": true,
	}
	if !validTimeRanges[c.Activity.TimeRange] {
		return fmt.Errorf(
			"config: invalid activity time_range %q",
			c.Activity.TimeRange,
		)
	}
	if c.Activity.DefaultBranchRetentionDays == 0 {
		c.Activity.DefaultBranchRetentionDays = defaultBranchActivityRetentionDays
	}
	if c.Activity.DefaultBranchRetentionDays < 0 {
		return fmt.Errorf(
			"config: activity.default_branch_retention_days must be positive or omitted, got %d",
			c.Activity.DefaultBranchRetentionDays,
		)
	}
	if c.Activity.DefaultBranchMaxCommits == 0 {
		c.Activity.DefaultBranchMaxCommits = defaultBranchActivityMaxCommits
	}
	if c.Activity.DefaultBranchMaxCommits < 0 {
		return fmt.Errorf(
			"config: activity.default_branch_max_commits must be positive or omitted, got %d",
			c.Activity.DefaultBranchMaxCommits,
		)
	}
	if c.Detail.InitialTimelineEntryLimit == 0 {
		c.Detail.InitialTimelineEntryLimit = DefaultInitialTimelineEntryLimit
	}
	if c.Detail.InitialTimelineEntryLimit < MinInitialTimelineEntryLimit ||
		c.Detail.InitialTimelineEntryLimit > MaxInitialTimelineEntryLimit {
		return fmt.Errorf(
			"config: detail.initial_timeline_entry_limit must be between %d and %d or omitted, got %d",
			MinInitialTimelineEntryLimit,
			MaxInitialTimelineEntryLimit,
			c.Detail.InitialTimelineEntryLimit,
		)
	}

	c.IssueWorkspaceBranchStyle = strings.TrimSpace(c.IssueWorkspaceBranchStyle)
	if c.IssueWorkspaceBranchStyle == "" {
		c.IssueWorkspaceBranchStyle = defaultIssueWorkspaceBranchStyle
	}
	switch c.IssueWorkspaceBranchStyle {
	case IssueWorkspaceBranchStyleSlug, IssueWorkspaceBranchStyleBare:
	default:
		return fmt.Errorf(
			"config: invalid issue_workspace_branch_style %q: must be %q or %q",
			c.IssueWorkspaceBranchStyle,
			IssueWorkspaceBranchStyleSlug,
			IssueWorkspaceBranchStyleBare,
		)
	}

	c.Terminal.FontFamily = strings.TrimSpace(c.Terminal.FontFamily)
	if c.Terminal.FontSize == 0 {
		c.Terminal.FontSize = DefaultTerminalFontSize
	}
	if c.Terminal.FontSize < 8 || c.Terminal.FontSize > 32 {
		return fmt.Errorf(
			"config: invalid terminal.font_size %d: must be between 8 and 32",
			c.Terminal.FontSize,
		)
	}
	if c.Terminal.Scrollback == 0 {
		c.Terminal.Scrollback = DefaultTerminalScrollback
	}
	if c.Terminal.Scrollback < 100 || c.Terminal.Scrollback > 100000 {
		return fmt.Errorf(
			"config: invalid terminal.scrollback %d: must be between 100 and 100000",
			c.Terminal.Scrollback,
		)
	}
	if c.Terminal.LineHeight == 0 {
		c.Terminal.LineHeight = DefaultTerminalLineHeight
	}
	if c.Terminal.LineHeight < 0.8 || c.Terminal.LineHeight > 2 {
		return fmt.Errorf(
			"config: invalid terminal.line_height %.2f: must be between 0.8 and 2",
			c.Terminal.LineHeight,
		)
	}
	if c.Terminal.LetterSpacing < -2 || c.Terminal.LetterSpacing > 8 {
		return fmt.Errorf(
			"config: invalid terminal.letter_spacing %d: must be between -2 and 8",
			c.Terminal.LetterSpacing,
		)
	}
	if c.Terminal.CursorBlink == nil {
		cursorBlink := DefaultTerminalCursorBlink
		c.Terminal.CursorBlink = &cursorBlink
	}
	if c.Terminal.Graphics == nil {
		graphics := DefaultTerminalGraphics
		c.Terminal.Graphics = &graphics
	}
	if c.Terminal.TmuxMouse == nil {
		tmuxMouse := DefaultTerminalTmuxMouse
		c.Terminal.TmuxMouse = &tmuxMouse
	}
	if c.Terminal.RetainedSessions == nil {
		retainedSessions := DefaultTerminalRetainedSessions
		c.Terminal.RetainedSessions = &retainedSessions
	}
	if *c.Terminal.RetainedSessions < 0 || *c.Terminal.RetainedSessions > 20 {
		return fmt.Errorf(
			"config: invalid terminal.retained_sessions %d: must be between 0 and 20",
			*c.Terminal.RetainedSessions,
		)
	}
	if err := c.validateAgents(); err != nil {
		return err
	}

	if len(c.Tmux.Command) > 0 &&
		strings.TrimSpace(c.Tmux.Command[0]) == "" {
		return fmt.Errorf(
			"config: invalid tmux.command: first element must be non-empty",
		)
	}

	if len(c.Shell.Command) > 0 &&
		strings.TrimSpace(c.Shell.Command[0]) == "" {
		return fmt.Errorf(
			"config: invalid shell.command: first element must be non-empty",
		)
	}

	return nil
}

func normalizePlatformTransport(p *PlatformConfig) error {
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	if p.Type != string(platformpkg.KindGitea) {
		if p.BaseURL != "" || p.AllowInsecure {
			return fmt.Errorf("base_url and allow_insecure are supported only for gitea")
		}
		return nil
	}
	if p.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("base_url must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("base_url must not include user info")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("base_url must not include a query string")
	}
	if u.Fragment != "" {
		return fmt.Errorf("base_url must not include a fragment")
	}
	if u.Scheme == "http" && !p.AllowInsecure {
		return fmt.Errorf("base_url uses plain HTTP; set allow_insecure = true to acknowledge that API tokens will be sent without TLS")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	p.BaseURL = u.String()
	return nil
}

// Validate checks and normalizes the Tailscale Serve user allowlist.
func (t *TailscaleServeAPI) Validate() error {
	seen := make(map[string]struct{}, len(t.AllowedUsers))
	for index, raw := range t.AllowedUsers {
		login, err := NormalizeTailscaleLogin(raw)
		if err != nil {
			return fmt.Errorf("api.tailscale_serve.allowed_users[%d]: %w", index, err)
		}
		if _, duplicate := seen[login]; duplicate {
			return fmt.Errorf(
				"api.tailscale_serve.allowed_users contains duplicate login %q",
				login,
			)
		}
		seen[login] = struct{}{}
		t.AllowedUsers[index] = login
	}
	if t.Enabled && len(t.AllowedUsers) == 0 {
		return errors.New("api.tailscale_serve.enabled requires at least one allowed user")
	}
	return nil
}

// NormalizeTailscaleLogin returns the exact case-insensitive login identity
// used by Tailscale Serve. Forge accepts the email-shaped login names emitted
// for user-owned tailnet devices, not display-name or list syntax.
func NormalizeTailscaleLogin(raw string) (string, error) {
	login := strings.ToLower(strings.TrimSpace(raw))
	if login == "" || len(login) > 320 || strings.ContainsAny(login, " ,\t\r\n") {
		return "", errors.New("login must be one email-shaped identity")
	}
	if strings.Count(login, "@") != 1 {
		return "", errors.New("login must be one email-shaped identity")
	}
	at := strings.IndexByte(login, '@')
	if at <= 0 || at == len(login)-1 {
		return "", errors.New("login must be one email-shaped identity")
	}
	return login, nil
}

// Validate checks and canonicalizes the fleet role and membership.
func (f *Fleet) Validate() error {
	role := f.RoleOrDefault()
	if role != FleetRoleHub && role != FleetRoleSpoke {
		return fmt.Errorf("fleet.role must be %q or %q, got %q", FleetRoleHub, FleetRoleSpoke, f.Role)
	}
	if f.Role != "" {
		f.Role = role
	}
	f.BaseURL = strings.TrimSpace(f.BaseURL)
	if f.Enabled && f.BaseURL == "" {
		return errors.New("fleet.base_url is required when federation is enabled")
	}
	if f.BaseURL != "" {
		baseURL, err := federation.CanonicalOrigin(f.BaseURL)
		if err != nil {
			return fmt.Errorf("fleet.base_url: %w", err)
		}
		f.BaseURL = baseURL
	}
	if role == FleetRoleSpoke && f.Hub == nil {
		return errors.New("fleet.hub is required when fleet.role is spoke")
	}
	if f.Hub != nil {
		f.Hub.NodeID = strings.TrimSpace(f.Hub.NodeID)
		f.Hub.Name = strings.TrimSpace(f.Hub.Name)
		f.Hub.BaseURL = strings.TrimSpace(f.Hub.BaseURL)
		if !validFleetNodeID(f.Hub.NodeID) {
			return errors.New("fleet.hub.node_id must be 32 lowercase hexadecimal characters")
		}
		baseURL, err := federation.CanonicalOrigin(f.Hub.BaseURL)
		if err != nil {
			return fmt.Errorf("fleet.hub.base_url: %w", err)
		}
		f.Hub.BaseURL = baseURL
	}
	if role == FleetRoleSpoke && len(f.Members) > 0 {
		return errors.New("fleet.members is only valid when fleet.role is hub")
	}
	memberIDs := make(map[string]struct{}, len(f.Members))
	memberOrigins := make(map[string]struct{}, len(f.Members))
	for index := range f.Members {
		member := &f.Members[index]
		member.NodeID = strings.TrimSpace(member.NodeID)
		member.Name = strings.TrimSpace(member.Name)
		member.State = federation.EnrollmentState(strings.TrimSpace(string(member.State)))
		if !validFleetNodeID(member.NodeID) {
			return fmt.Errorf("fleet.members[%d].node_id must be 32 lowercase hexadecimal characters", index)
		}
		if member.State != federation.EnrollmentActive {
			return fmt.Errorf("fleet.members[%d].state must be %q", index, federation.EnrollmentActive)
		}
		baseURL, err := federation.CanonicalOrigin(member.BaseURL)
		if err != nil {
			return fmt.Errorf("fleet.members[%d].base_url: %w", index, err)
		}
		member.BaseURL = baseURL
		if _, duplicate := memberIDs[member.NodeID]; duplicate {
			return fmt.Errorf("fleet.members contains duplicate node ID %q", member.NodeID)
		}
		memberIDs[member.NodeID] = struct{}{}
		if _, duplicate := memberOrigins[member.BaseURL]; duplicate {
			return fmt.Errorf("fleet.members contains duplicate origin %q", member.BaseURL)
		}
		memberOrigins[member.BaseURL] = struct{}{}
	}
	if f.PeerTimeout != "" {
		peerTimeout, err := time.ParseDuration(f.PeerTimeout)
		if err != nil {
			return fmt.Errorf("fleet.peer_timeout %q: %w", f.PeerTimeout, err)
		}
		if peerTimeout <= 0 {
			return errors.New("fleet.peer_timeout must be positive")
		}
	}
	return nil
}

func validFleetNodeID(nodeID string) bool {
	if len(nodeID) != 32 {
		return false
	}
	for _, char := range nodeID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// docFolderIDPattern constrains a docs folder id to characters that
// survive as a single URL path segment, matching docs.ValidateFolderID.
// Duplicated here because internal/docs imports this package.
var docFolderIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (c *Config) canonicalizeDocFolders() error {
	seen := make(map[string]struct{}, len(c.DocFolders))
	for i := range c.DocFolders {
		folder := &c.DocFolders[i]
		folder.ID = strings.TrimSpace(folder.ID)
		if folder.ID == "" {
			return fmt.Errorf("config: doc_folders[%d]: id is required", i)
		}
		if folder.ID == "." || folder.ID == ".." || !docFolderIDPattern.MatchString(folder.ID) {
			return fmt.Errorf("config: doc_folders[%q]: id may contain only letters, digits, '.', '_' or '-'", folder.ID)
		}
		if _, dup := seen[folder.ID]; dup {
			return fmt.Errorf("config: doc_folders: duplicate id %q", folder.ID)
		}
		seen[folder.ID] = struct{}{}

		folder.Path = strings.TrimSpace(folder.Path)
		if folder.Path == "" {
			return fmt.Errorf("config: doc_folders[%q]: path is required", folder.ID)
		}
		expanded, err := expandTilde(folder.Path)
		if err != nil {
			return fmt.Errorf("config: doc_folders[%q]: %w", folder.ID, err)
		}
		resolved, err := filepath.Abs(expanded)
		if err != nil {
			return fmt.Errorf("config: doc_folders[%q]: resolve path: %w", folder.ID, err)
		}
		folder.Path = resolved

		folder.Name = strings.TrimSpace(folder.Name)
		if folder.Name == "" {
			folder.Name = filepath.Base(resolved)
		}
		folder.Daemon = strings.TrimSpace(folder.Daemon)
	}
	return nil
}

func expandTilde(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := homeDir()
		if home == "" {
			return "", errors.New("home directory is not set")
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func (c *Config) validatePlatforms() error {
	seen := make(map[string]tokenauth.Descriptor, len(c.Platforms))
	for _, p := range c.Platforms {
		key := p.Type + "\x00" + p.Host
		display := p.Type + "/" + p.Host
		desc := tokenauth.Descriptor{Key: tokenauth.Key{Platform: p.Type, Host: p.Host}}
		if p.TokenFile != "" {
			desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
				Kind:     tokenauth.SourceKindFile,
				FilePath: p.TokenFile,
			})
		}
		if p.TokenEnv != "" {
			desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
				Kind:    tokenauth.SourceKindEnv,
				EnvName: p.TokenEnv,
			})
		}
		if prev, ok := seen[key]; ok {
			if prev.EqualSource(desc) {
				return fmt.Errorf("config: duplicate platform %q", display)
			}
			return fmt.Errorf(
				"config: conflicting token source for platform %q (conflicting token_env): %s vs %s",
				display, prev.SafeString(), desc.SafeString(),
			)
		}
		seen[key] = desc
	}
	return nil
}

// validateTokenEnvNamesNotTerminalVars rejects token env names that
// collide with the non-secret environment passed to terminal sessions.
// The tmux server permanently retains its spawn environment, so a
// variable on that list could never be scrubbed once declared secret;
// rejecting the collision keeps the two contracts disjoint by
// construction.
func (c *Config) validateTokenEnvNamesNotTerminalVars() error {
	for _, name := range c.TokenEnvNames() {
		if IsTmuxNonSecretEnvVar(strings.TrimSpace(name)) {
			return fmt.Errorf(
				"config: token env name %q collides with the non-secret "+
					"environment passed to terminal sessions; use a "+
					"dedicated variable name",
				name,
			)
		}
	}
	return nil
}

func (c *Config) validateGitHubOwnerTokens() error {
	seen := make(map[string]struct{}, len(c.GitHubOwnerTokens))
	for i := range c.GitHubOwnerTokens {
		item := &c.GitHubOwnerTokens[i]
		item.Owner = strings.TrimSpace(item.Owner)
		item.TokenEnv = strings.TrimSpace(item.TokenEnv)
		item.TokenFile = strings.TrimSpace(item.TokenFile)
		if item.Owner == "" {
			return fmt.Errorf(
				"config: github_owner_tokens[%d]: owner is required", i,
			)
		}
		if strings.Contains(item.Owner, "/") || strings.ContainsAny(item.Owner, "*?[") {
			return fmt.Errorf(
				"config: github_owner_tokens[%d]: owner must be one exact GitHub owner", i,
			)
		}
		host, err := normalizePlatformHost(defaultPlatform, item.Host)
		if err != nil {
			return fmt.Errorf("config: github_owner_tokens[%d]: %w", i, err)
		}
		item.Host = host
		if item.TokenFile == "" && item.TokenEnv == "" {
			return fmt.Errorf(
				"config: github_owner_tokens[%d]: token_file or token_env is required", i,
			)
		}
		owner := strings.ToLower(item.Owner)
		key := strings.ToLower(host) + "\x00" + owner
		if _, ok := seen[key]; ok {
			return fmt.Errorf(
				"config: github_owner_tokens[%d]: duplicate github owner token for host %q and owner %q",
				i, host, owner,
			)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (c *Config) validateGitHubApps() error {
	seenOwners := make(map[string]struct{}, len(c.GitHubApps))
	seenInstallations := make(map[string]struct{}, len(c.GitHubApps))
	seenInstallationIdentities := make(map[string]string, len(c.GitHubApps))
	for i := range c.GitHubApps {
		app := &c.GitHubApps[i]
		app.Host = strings.TrimSpace(app.Host)
		app.Role = strings.ToLower(strings.TrimSpace(app.Role))
		if app.Role == "" {
			app.Role = GitHubAppRoleSync
		}
		if app.Role != GitHubAppRoleSync && app.Role != GitHubAppRoleArchive {
			return fmt.Errorf(
				"config: github_apps[%d]: role must be %q or %q (got %q)",
				i, GitHubAppRoleSync, GitHubAppRoleArchive, app.Role,
			)
		}
		app.Slug = strings.TrimSpace(app.Slug)
		app.Owner = strings.TrimSpace(app.Owner)
		app.PrivateKeyPath = strings.TrimSpace(app.PrivateKeyPath)
		app.InstallationAccount = strings.TrimSpace(app.InstallationAccount)
		host, err := normalizePlatformHost(defaultPlatform, app.Host)
		if err != nil {
			return fmt.Errorf("config: github_apps[%d]: %w", i, err)
		}
		app.Host = host
		if app.AppID <= 0 {
			return fmt.Errorf(
				"config: github_apps[%d]: app_id must be a positive integer", i,
			)
		}
		if app.PrivateKeyPath == "" {
			return fmt.Errorf(
				"config: github_apps[%d]: private_key_path is required", i,
			)
		}
		// An installation without its account cannot be scoped to repository
		// owners safely.
		if app.InstallationID != 0 && app.InstallationAccount == "" {
			return fmt.Errorf(
				"config: github_apps[%d]: installation_account is required when "+
					"installation_id is set", i,
			)
		}
		app.RepositorySelection = strings.ToLower(strings.TrimSpace(app.RepositorySelection))
		switch app.RepositorySelection {
		case "", "all", "selected":
		default:
			return fmt.Errorf(
				"config: github_apps[%d]: repository_selection must be \"all\" or "+
					"\"selected\" (got %q)", i, app.RepositorySelection,
			)
		}
		if app.InstallationID != 0 && app.RepositorySelection == "" {
			return fmt.Errorf(
				"config: github_apps[%d]: repository_selection is required when "+
					"installation_id is set", i,
			)
		}
		// GitHub App credentials are host-shaped, but private apps are owned by
		// one account. Multiple rows may share a host only when they describe
		// distinct apps, selected by app owner or installation account.
		owner := strings.ToLower(app.Owner)
		if owner == "" {
			owner = fmt.Sprintf("app:%d", app.AppID)
		}
		ownerKey := host + "\x00" + app.Role + "\x00" + owner
		if _, ok := seenOwners[ownerKey]; ok {
			label := app.Owner
			if label == "" {
				label = fmt.Sprintf("app:%d", app.AppID)
			}
			return fmt.Errorf(
				"config: github_apps[%d]: duplicate github app for host %q and owner %q",
				i, host, label,
			)
		}
		seenOwners[ownerKey] = struct{}{}
		if app.InstallationAccount != "" {
			installKey := host + "\x00" + app.Role + "\x00" + strings.ToLower(app.InstallationAccount)
			if _, ok := seenInstallations[installKey]; ok {
				return fmt.Errorf(
					"config: github_apps[%d]: duplicate github app installation for host %q and account %q",
					i, host, app.InstallationAccount,
				)
			}
			seenInstallations[installKey] = struct{}{}
			identityKey := host + "\x00" + strconv.FormatInt(app.AppID, 10) +
				"\x00" + strconv.FormatInt(app.InstallationID, 10) +
				"\x00" + strings.ToLower(app.InstallationAccount)
			if previousRole, ok := seenInstallationIdentities[identityKey]; ok &&
				previousRole != app.Role {
				return fmt.Errorf(
					"config: github_apps[%d]: installation for host %q, app id %d, and account %q cannot be configured for both %q and %q roles",
					i, host, app.AppID, app.InstallationAccount,
					previousRole, app.Role,
				)
			}
			seenInstallationIdentities[identityKey] = app.Role
		}
	}
	return nil
}

// GitHubAppsForHost returns the configured GitHub App installations for host.
func (c *Config) GitHubAppsForHost(host string) []GitHubAppConfig {
	if c == nil {
		return nil
	}
	h, err := normalizePlatformHost(defaultPlatform, host)
	if err != nil {
		return nil
	}
	var apps []GitHubAppConfig
	for _, app := range c.GitHubApps {
		if app.Host == h {
			apps = append(apps, app)
		}
	}
	return apps
}

// GitHubAppForHost returns a configured GitHub App credential for host, if any.
// Callers that manage app state should use GitHubAppsForHost and disambiguate
// when multiple app credentials share one host.
func (c *Config) GitHubAppForHost(host string) (GitHubAppConfig, bool) {
	apps := c.GitHubAppsForHost(host)
	if len(apps) == 0 {
		return GitHubAppConfig{}, false
	}
	return apps[0], true
}

func (c *Config) validateAgents() error {
	seen := make(map[string]struct{}, len(c.Agents))
	for i := range c.Agents {
		agent := &c.Agents[i]
		agent.Key = strings.ToLower(strings.TrimSpace(agent.Key))
		agent.Label = strings.TrimSpace(agent.Label)
		if agent.Key == "" {
			return fmt.Errorf("config: agents[%d]: key is required", i)
		}
		if agent.Label == "" {
			agent.Label = agent.Key
		}
		if reservedSystemLaunchTargetKeys[agent.Key] {
			return fmt.Errorf(
				"config: agents[%d]: key %q is a reserved system launch target",
				i, agent.Key,
			)
		}
		if _, ok := seen[agent.Key]; ok {
			return fmt.Errorf(
				"config: duplicate agent %q", agent.Key,
			)
		}
		seen[agent.Key] = struct{}{}

		if !agent.EnabledOrDefault() {
			continue
		}
		if len(agent.Command) == 0 {
			return fmt.Errorf(
				"config: agents[%d]: command is required when enabled", i,
			)
		}
		if strings.TrimSpace(agent.Command[0]) == "" {
			return fmt.Errorf(
				"config: agents[%d]: command first element must be non-empty", i,
			)
		}
	}
	return nil
}

func repoIdentityKey(r Repo) string {
	return strings.Join([]string{
		r.PlatformOrDefault(),
		r.PlatformHostOrDefault(),
		strings.ToLower(repoPathOrFullName(r)),
	}, "\x00")
}

func repoIdentityDisplay(r Repo) string {
	platform := r.PlatformOrDefault()
	host := r.PlatformHostOrDefault()
	if platform == defaultPlatform && host == defaultPlatformHost {
		return repoPathOrFullName(r)
	}
	return platform + "/" + host + "/" + repoPathOrFullName(r)
}

func repoPathOrFullName(r Repo) string {
	if strings.TrimSpace(r.RepoPath) != "" {
		return strings.TrimSpace(r.RepoPath)
	}
	return r.Owner + "/" + r.Name
}

func kataProjectMappingKey(mapping KataProjectRepoMapping) string {
	return strings.TrimSpace(mapping.DaemonID) + "\x00" + strings.TrimSpace(mapping.ProjectUID)
}

func (c *Config) validateKataProjectRepoMappings() error {
	seen := make(map[string]struct{}, len(c.KataProjects))
	for i := range c.KataProjects {
		mapping := &c.KataProjects[i]
		mapping.DaemonID = strings.TrimSpace(mapping.DaemonID)
		mapping.ProjectUID = strings.TrimSpace(mapping.ProjectUID)
		if mapping.ProjectUID == "" {
			return fmt.Errorf("config: kata_projects[%d]: project_uid is required", i)
		}
		normalizedProvider, err := normalizePlatform(mapping.Provider)
		if err != nil {
			return fmt.Errorf("config: kata_projects[%d]: provider: %w", i, err)
		}
		mapping.Provider = normalizedProvider
		mapping.PlatformHost, err = normalizePlatformHost(mapping.Provider, mapping.PlatformHost)
		if err != nil {
			return fmt.Errorf("config: kata_projects[%d]: platform_host: %w", i, err)
		}

		repo := Repo{
			Platform:     mapping.Provider,
			PlatformHost: mapping.PlatformHost,
			RepoPath:     mapping.RepoPath,
		}
		if err := repo.normalize(c.DefaultPlatformHost); err != nil {
			return fmt.Errorf("config: kata_projects[%d]: repo_path: %w", i, err)
		}
		if repo.HasNameGlob() {
			return fmt.Errorf(
				"config: kata_projects[%d]: repo_path does not match a configured exact repo",
				i,
			)
		}
		mapping.RepoPath = repoPathOrFullName(repo)

		dupKey := kataProjectMappingKey(*mapping)
		if _, ok := seen[dupKey]; ok {
			return fmt.Errorf(
				"config: kata_projects[%d]: duplicate kata project mapping for daemon %q project %q",
				i,
				mapping.DaemonID,
				mapping.ProjectUID,
			)
		}
		seen[dupKey] = struct{}{}

	}
	return nil
}

var reservedSystemLaunchTargetKeys = map[string]bool{
	"tmux":        true,
	"plain_shell": true,
	"shell":       true,
}

var (
	validBasePathRe = regexp.MustCompile(`^/([a-zA-Z0-9._~-]+/)*$`)
	// Without scheme: require / so bare "github.com" (a valid repo
	// name) is not falsely matched.
	bareHostRepoRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9.-]*(?:\.[A-Za-z0-9.-]+|:[0-9]+))/(.*)$`)
	// SCP-style only (git@host:path); ssh:// URIs use net/url.
	scpRepoRe = regexp.MustCompile(`^[^@]+@([^:]+):(.*)$`)
)

func (c *Config) SyncDuration() time.Duration {
	d, _ := time.ParseDuration(c.SyncInterval)
	return d
}

func (c *Config) ActivePRRefreshDuration() time.Duration {
	if c == nil || c.ActivePRRefreshInterval == "" {
		d, _ := time.ParseDuration(defaultActivePRRefreshInterval)
		return d
	}
	d, _ := time.ParseDuration(c.ActivePRRefreshInterval)
	return d
}

func (c *Config) ActivePRWindowDuration() time.Duration {
	if c == nil || c.ActivePRWindow == "" {
		d, _ := time.ParseDuration(defaultActivePRWindow)
		return d
	}
	d, _ := time.ParseDuration(c.ActivePRWindow)
	return d
}

func (c *Config) BranchActivityRetention() time.Duration {
	if c == nil || c.Activity.DefaultBranchRetentionDays <= 0 {
		return time.Duration(defaultBranchActivityRetentionDays) * 24 * time.Hour
	}
	return time.Duration(c.Activity.DefaultBranchRetentionDays) * 24 * time.Hour
}

// NotificationsEnabled reports whether notification sync and the
// notification API are active. Notifications are a built-in capability with
// no enable/disable setting; the only "off" state is the absence of any
// loaded config (nil), which the callers use for nil-safety.
func (c *Config) NotificationsEnabled() bool {
	return c != nil
}

func (c *Config) NotificationSyncDuration() time.Duration {
	d, _ := time.ParseDuration(c.Notifications.SyncInterval)
	return d
}

func (c *Config) NotificationPropagationDuration() time.Duration {
	d, _ := time.ParseDuration(c.Notifications.PropagationInterval)
	return d
}

func (c *Config) NotificationBatchSize() int {
	if c.Notifications.BatchSize <= 0 {
		return 25
	}
	return c.Notifications.BatchSize
}

func (c *Config) GitHubToken() string {
	return c.gitHubTokenForHost(platformpkg.DefaultGitHubHost)
}

// gitHubTokenForHost resolves a github token for a specific host. The
// configured GitHubTokenEnv env var wins when non-empty, but only for
// github.com — it holds the public-GitHub token and must not leak to
// Enterprise/self-hosted hosts. Every host falls back to the
// host-scoped `gh auth token --hostname <host>`. Internal callers go
// through GitHubToken() or TokenForPlatformHost.
func (c *Config) gitHubTokenForHost(host string) string {
	if host == platformpkg.DefaultGitHubHost {
		if token := os.Getenv(c.GitHubTokenEnv); token != "" {
			return token
		}
	}
	return ghAuthTokenForHost(host)
}

func (c *Config) TokenForPlatformHost(platform, host, repoTokenEnv string) string {
	if c == nil {
		return ""
	}
	if repoTokenEnv != "" {
		if token := os.Getenv(repoTokenEnv); token != "" {
			return token
		}
	}
	p, err := normalizePlatform(platform)
	if err != nil {
		return ""
	}
	h, err := normalizePlatformHost(p, host)
	if err != nil {
		return ""
	}
	for _, pc := range c.Platforms {
		if pc.Type == p && pc.Host == h && pc.TokenEnv != "" {
			return os.Getenv(pc.TokenEnv)
		}
	}
	if defaultTokenEnv, ok := defaultTokenEnvForPlatformHost(p, h); ok {
		return os.Getenv(defaultTokenEnv)
	}
	if p == defaultPlatform {
		return c.gitHubTokenForHost(h)
	}
	return ""
}

func (c *Config) ResolveRepoToken(r Repo) string {
	if c == nil {
		return r.ResolveToken("")
	}
	return c.TokenForPlatformHost(
		r.PlatformOrDefault(), r.PlatformHostOrDefault(), r.TokenEnv,
	)
}

// ConfiguredCredentialAvailable reports whether any credential route this
// config declares resolves to a credential right now. It walks the same plans
// provider startup registers, so it sees standalone owner PAT and App
// installation routes that no [[repos]] entry names — those routes still serve
// owner discovery and repository import.
//
// A readable App private key counts, since it mints installation tokens on
// demand. The `gh` CLI candidate is deliberately excluded: it shells out and is
// host-scoped, so pollers resolve it once per host through TokenForPlatformHost
// instead of once per route here.
// selectedRepoUnderAccount splits one selected_repos entry and reports whether
// it names a repository the installation account owns. App route generation and
// coverage checks must agree on this predicate: an entry with a blank repository
// name (for example "acme/") creates no route, so it must not count as coverage
// either.
func selectedRepoUnderAccount(
	entry, installationAccount string,
) (string, string, bool) {
	owner, name, ok := strings.Cut(strings.TrimSpace(entry), "/")
	if !ok || strings.TrimSpace(name) == "" ||
		!strings.EqualFold(owner, installationAccount) {
		return "", "", false
	}
	return owner, name, true
}

// globServedBySelectedApp reports whether repo is a name pattern whose
// repositories a selected-repository App installation already covers. Such a
// pattern gets an owner-scoped route because the App cannot cover the literal
// pattern name, and requiring that route would fail startup for an App-only
// configuration even though the App's own exact routes serve every repository
// the pattern expands to. The route stays in the plan list so a PAT still
// broadens discovery when one is configured; it simply must not be mandatory.
func (c *Config) globServedBySelectedApp(repo Repo) bool {
	if c == nil || !repo.nameHasGlob() {
		return false
	}
	host := repo.PlatformHostOrDefault()
	if repo.PlatformOrDefault() != defaultPlatform {
		return false
	}
	for _, app := range c.GitHubAppsForHost(host) {
		if app.AppID <= 0 || app.InstallationID <= 0 ||
			app.PrivateKeyPath == "" ||
			githubAppRole(app) != GitHubAppRoleSync ||
			!strings.EqualFold(app.InstallationAccount, repo.Owner) ||
			!strings.EqualFold(strings.TrimSpace(app.RepositorySelection), "selected") {
			continue
		}
		for _, selected := range app.SelectedRepos {
			if _, _, ok := selectedRepoUnderAccount(selected, repo.Owner); ok {
				return true
			}
		}
	}
	return false
}

func (c *Config) ConfiguredCredentialAvailable() bool {
	if c == nil {
		return false
	}
	for _, plan := range c.ProviderTokenSources() {
		if descriptorCredentialAvailable(plan.Descriptor) {
			return true
		}
	}
	return false
}

func descriptorCredentialAvailable(desc tokenauth.Descriptor) bool {
	for _, candidate := range desc.Candidates {
		switch candidate.Kind {
		case tokenauth.SourceKindEnv:
			if os.Getenv(candidate.EnvName) != "" {
				return true
			}
		case tokenauth.SourceKindFile, tokenauth.SourceKindGitHubApp:
			data, err := os.ReadFile(candidate.FilePath)
			if err == nil && len(bytes.TrimSpace(data)) > 0 {
				return true
			}
		}
	}
	return false
}

func (c *Config) ResolveRepoTokenSource(r Repo) tokenauth.Descriptor {
	if c == nil {
		return tokenauth.Descriptor{}
	}
	if r.PlatformOrDefault() == defaultPlatform {
		return c.ResolveGitHubRepoTokenSource(r)
	}
	return c.TokenSourceForPlatformHost(
		r.PlatformOrDefault(), r.PlatformHostOrDefault(), r.TokenEnv, r.TokenFile,
	)
}

// HasExplicitGitHubTokenEnv reports whether github_token_env names a
// deliberately configured github.com fallback credential rather than
// kenn-forge's built-in default. Load defaults the field, and Save and the
// sample config both materialize the default name into the file, so the
// value itself is the only durable signal: only a non-default name is an
// explicit fallback choice.
func (c *Config) HasExplicitGitHubTokenEnv() bool {
	if c == nil {
		return false
	}
	env := strings.TrimSpace(c.GitHubTokenEnv)
	return env != "" && env != defaultGitHubTokenEnv
}

// GitHubOwnerTokenFor returns the exact owner PAT mapping for host and owner.
func (c *Config) GitHubOwnerTokenFor(
	host, owner string,
) (GitHubOwnerTokenConfig, bool) {
	if c == nil {
		return GitHubOwnerTokenConfig{}, false
	}
	h, err := normalizePlatformHost(defaultPlatform, host)
	if err != nil {
		return GitHubOwnerTokenConfig{}, false
	}
	for _, item := range c.GitHubOwnerTokens {
		if strings.EqualFold(item.Host, h) && strings.EqualFold(item.Owner, owner) {
			return item, true
		}
	}
	return GitHubOwnerTokenConfig{}, false
}

// ResolveGitHubRepoTokenSource builds the credential route for one GitHub
// repository. Repository overrides and selected-installation coverage are
// exact routes; otherwise repositories under one owner share the owner route.
//
// A covering App installation always leads the chain, including on a repository
// that also configures its own PAT. Installation tokens carry their own
// rate-limit budget, so reads prefer them in every case; a repository PAT is
// still the credential that serves that repository's writes, because mutation
// resolution skips App candidates to keep writes attributed to the user
// (tokenauth.WithMutationAuth). One ordered chain therefore expresses both:
// App-first for reads, override-first among the PATs for writes.
func (c *Config) ResolveGitHubRepoTokenSource(r Repo) tokenauth.Descriptor {
	if c == nil {
		return tokenauth.Descriptor{}
	}
	host := r.PlatformHostOrDefault()
	overridden := r.TokenFile != "" || r.TokenEnv != ""
	app, appCoversRepo := c.gitHubAppForRepo(host, r.Owner, r.Name)
	exact := overridden || (appCoversRepo &&
		strings.EqualFold(app.RepositorySelection, "selected"))
	desc := tokenauth.Descriptor{Key: tokenauth.Key{
		Platform: defaultPlatform,
		Host:     host,
		Scope:    githubCredentialScope(r.Owner, r.Name, exact),
	}}
	if appCoversRepo {
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind:                tokenauth.SourceKindGitHubApp,
			Host:                host,
			FilePath:            app.PrivateKeyPath,
			AppID:               app.AppID,
			InstallationID:      app.InstallationID,
			InstallationAccount: app.InstallationAccount,
		})
	}
	appendTokenFileEnvCandidates(&desc, r.TokenFile, r.TokenEnv)
	if ownerToken, ok := c.GitHubOwnerTokenFor(host, r.Owner); ok {
		appendTokenFileEnvCandidates(
			&desc, ownerToken.TokenFile, ownerToken.TokenEnv,
		)
	}
	c.appendPlatformTokenCandidates(&desc, defaultPlatform, host)
	c.appendGitHubDefaultCandidates(&desc, host)
	return desc
}

// ResolveGitHubArchiveTokenSource builds the optional, App-only credential
// route for archive reads of one GitHub repository. It deliberately has no
// PAT or gh fallback: when configured, archive work either uses the dedicated
// installation budget or fails closed instead of spending ordinary capacity.
func (c *Config) ResolveGitHubArchiveTokenSource(r Repo) tokenauth.Descriptor {
	if c == nil || r.PlatformOrDefault() != defaultPlatform {
		return tokenauth.Descriptor{}
	}
	host := r.PlatformHostOrDefault()
	app, covered := c.gitHubArchiveAppForRepo(host, r.Owner, r.Name)
	if !covered {
		return tokenauth.Descriptor{}
	}
	exact := strings.EqualFold(app.RepositorySelection, "selected")
	desc := tokenauth.Descriptor{Key: tokenauth.Key{
		Platform: defaultPlatform,
		Host:     host,
		Scope:    "archive:" + githubCredentialScope(r.Owner, r.Name, exact),
	}}
	desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
		Kind:                tokenauth.SourceKindGitHubApp,
		Host:                host,
		FilePath:            app.PrivateKeyPath,
		AppID:               app.AppID,
		InstallationID:      app.InstallationID,
		InstallationAccount: app.InstallationAccount,
	})
	return desc
}

func (c *Config) gitHubAppForRepo(
	host, owner, name string,
) (GitHubAppConfig, bool) {
	for _, app := range c.GitHubAppsForHost(host) {
		if app.AppID <= 0 || app.InstallationID <= 0 ||
			app.PrivateKeyPath == "" ||
			githubAppRole(app) != GitHubAppRoleSync ||
			!strings.EqualFold(app.InstallationAccount, owner) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(app.RepositorySelection)) {
		case "all":
			return app, true
		case "selected":
			fullName := strings.ToLower(strings.TrimSpace(owner)) + "/" +
				strings.ToLower(strings.TrimSpace(name))
			if strings.TrimSpace(name) == "" {
				continue
			}
			for _, selected := range app.SelectedRepos {
				if strings.EqualFold(strings.TrimSpace(selected), fullName) {
					return app, true
				}
			}
		}
	}
	return GitHubAppConfig{}, false
}

func (c *Config) gitHubArchiveAppForRepo(
	host, owner, name string,
) (GitHubAppConfig, bool) {
	for _, app := range c.GitHubAppsForHost(host) {
		if app.AppID <= 0 || app.InstallationID <= 0 ||
			app.PrivateKeyPath == "" ||
			githubAppRole(app) != GitHubAppRoleArchive ||
			!strings.EqualFold(app.InstallationAccount, owner) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(app.RepositorySelection)) {
		case "all":
			return app, true
		case "selected":
			fullName := strings.ToLower(strings.TrimSpace(owner)) + "/" +
				strings.ToLower(strings.TrimSpace(name))
			if strings.TrimSpace(name) == "" {
				continue
			}
			for _, selected := range app.SelectedRepos {
				if strings.EqualFold(strings.TrimSpace(selected), fullName) {
					return app, true
				}
			}
		}
	}
	return GitHubAppConfig{}, false
}

func githubCredentialScope(owner, name string, exact bool) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if exact {
		return "repo:" + owner + "/" + strings.ToLower(strings.TrimSpace(name))
	}
	return "owner:" + owner
}

func appendTokenFileEnvCandidates(
	desc *tokenauth.Descriptor, tokenFile, tokenEnv string,
) {
	if tokenFile != "" {
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind: tokenauth.SourceKindFile, FilePath: tokenFile,
		})
	}
	if tokenEnv != "" {
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind: tokenauth.SourceKindEnv, EnvName: tokenEnv,
		})
	}
}

func (c *Config) appendPlatformTokenCandidates(
	desc *tokenauth.Descriptor, platform, host string,
) {
	for _, pc := range c.Platforms {
		if pc.Type == platform && pc.Host == host {
			appendTokenFileEnvCandidates(desc, pc.TokenFile, pc.TokenEnv)
			return
		}
	}
}

func (c *Config) appendGitHubDefaultCandidates(
	desc *tokenauth.Descriptor, host string,
) {
	if c.GitHubTokenEnv != "" && host == platformpkg.DefaultGitHubHost {
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind: tokenauth.SourceKindEnv, EnvName: c.GitHubTokenEnv,
		})
	}
	desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
		Kind: tokenauth.SourceKindGitHubCLI, Host: host,
	})
}

type ProviderTokenSource struct {
	Descriptor        tokenauth.Descriptor
	ArchiveDescriptor tokenauth.Descriptor
	Required          bool
	// ArchiveOnly marks a route whose ordinary credential chain is not
	// required for archive work. The startup path still registers the ordinary
	// source so the host provider exists, but it must not make an absent PAT
	// disable the independent archive App route.
	ArchiveOnly bool
	GitHubOwner string
}

func (c *Config) ProviderTokenSources() []ProviderTokenSource {
	if c == nil {
		return nil
	}
	seen := make(map[tokenauth.Key]struct{}, len(c.Repos)+len(c.Platforms)+1)
	out := make([]ProviderTokenSource, 0, len(c.Repos)+len(c.Platforms)+1)
	add := func(desc tokenauth.Descriptor, required bool) {
		if desc.Key.Host == "" {
			return
		}
		if _, ok := seen[desc.Key]; ok {
			return
		}
		// Optional hosts stay in the list even with no candidates: config
		// reload updates live sources from these plans, so dropping a host
		// whose token config was removed would leave its old credential
		// active until restart. Consumers that need a usable credential
		// resolve the source and skip optional misses.
		seen[desc.Key] = struct{}{}
		out = append(out, ProviderTokenSource{
			Descriptor: desc,
			Required:   required,
		})
	}
	for _, repo := range c.Repos {
		desc := c.ResolveRepoTokenSource(repo)
		archiveDesc := c.ResolveGitHubArchiveTokenSource(repo)
		// A selected archive installation is repository-exact. Keep the
		// ordinary route exact too, so several selected archive repositories
		// cannot collapse onto one owner route while startup builds the paired
		// archive routes.
		if strings.HasPrefix(archiveDesc.Key.Scope, "archive:repo:") &&
			!strings.HasPrefix(desc.Key.Scope, "repo:") {
			desc.Key.Scope = strings.TrimPrefix(archiveDesc.Key.Scope, "archive:")
		}
		plan := ProviderTokenSource{
			Descriptor:        desc,
			ArchiveDescriptor: archiveDesc,
			Required:          !c.globServedBySelectedApp(repo),
			ArchiveOnly:       archiveDesc.Key.Host != "" && !desc.HasActiveGitHubApp(),
		}
		if repo.PlatformOrDefault() == defaultPlatform {
			plan.GitHubOwner = repo.Owner
		}
		key := plan.Descriptor.Key
		if key.Host == "" {
			continue
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			out = append(out, plan)
			continue
		}
		// Keep same-host GitHub repo plans after the first so startup validates
		// each owner against the owner-scoped app/PAT chain.
		if plan.GitHubOwner != "" {
			out = append(out, plan)
		}
	}
	for _, app := range c.GitHubApps {
		if app.InstallationID <= 0 || strings.TrimSpace(app.InstallationAccount) == "" {
			continue
		}
		if strings.EqualFold(app.RepositorySelection, "selected") {
			for _, fullName := range app.SelectedRepos {
				owner, name, ok := selectedRepoUnderAccount(
					fullName, app.InstallationAccount,
				)
				if !ok {
					continue
				}
				repo := Repo{
					Platform: defaultPlatform, PlatformHost: app.Host,
					Owner: owner, Name: name,
				}
				desc := c.ResolveGitHubRepoTokenSource(repo)
				archiveDesc := c.ResolveGitHubArchiveTokenSource(repo)
				if githubAppRole(app) == GitHubAppRoleArchive &&
					archiveDesc.Key.Host == "" {
					continue
				}
				if strings.HasPrefix(archiveDesc.Key.Scope, "archive:repo:") &&
					!strings.HasPrefix(desc.Key.Scope, "repo:") {
					desc.Key.Scope = strings.TrimPrefix(archiveDesc.Key.Scope, "archive:")
				}
				if _, ok := seen[desc.Key]; ok {
					continue
				}
				seen[desc.Key] = struct{}{}
				out = append(out, ProviderTokenSource{
					Descriptor:        desc,
					ArchiveDescriptor: archiveDesc,
					Required:          true,
					ArchiveOnly:       archiveDesc.Key.Host != "" && !desc.HasActiveGitHubApp(),
					GitHubOwner:       owner,
				})
			}
			continue
		}
		repo := Repo{
			Platform:     defaultPlatform,
			PlatformHost: app.Host,
			Owner:        app.InstallationAccount,
		}
		desc := c.ResolveGitHubRepoTokenSource(repo)
		archiveDesc := c.ResolveGitHubArchiveTokenSource(repo)
		if githubAppRole(app) == GitHubAppRoleArchive && archiveDesc.Key.Host == "" {
			continue
		}
		if _, ok := seen[desc.Key]; ok {
			continue
		}
		seen[desc.Key] = struct{}{}
		out = append(out, ProviderTokenSource{
			Descriptor:        desc,
			ArchiveDescriptor: archiveDesc,
			Required:          true,
			ArchiveOnly:       archiveDesc.Key.Host != "" && !desc.HasActiveGitHubApp(),
			GitHubOwner:       app.InstallationAccount,
		})
	}
	for _, ownerToken := range c.GitHubOwnerTokens {
		repo := Repo{
			Platform:     defaultPlatform,
			PlatformHost: ownerToken.Host,
			Owner:        ownerToken.Owner,
		}
		desc := c.ResolveGitHubRepoTokenSource(repo)
		if _, ok := seen[desc.Key]; ok {
			continue
		}
		seen[desc.Key] = struct{}{}
		out = append(out, ProviderTokenSource{
			Descriptor:  desc,
			Required:    true,
			GitHubOwner: ownerToken.Owner,
		})
	}
	for _, p := range c.Platforms {
		add(c.TokenSourceForPlatformHost(p.Type, p.Host, "", ""), false)
	}
	add(c.TokenSourceForPlatformHost(
		defaultPlatform, platformpkg.DefaultGitHubHost, "", "",
	), false)
	return out
}

// CloneTokenDescriptors returns one descriptor per platform host carrying the
// host's ownerless git fallback chain under tokenauth.CloneKey(host).
// Repository-scoped Git operations select credentials by (provider, host,
// owner, name) and never consult this fallback; it exists only for genuinely
// ownerless host operations. When every tokened provider on a hostname agrees
// on one canonical chain, that chain is the fallback; when providers disagree,
// the fallback is disabled (empty chain) because an ownerless operation cannot
// select a provider safely. Hosts whose plans are all credential-less also
// keep an empty chain so a reload clears a previously tokened live clone
// source instead of leaving the removed credential active.
func (c *Config) CloneTokenDescriptors() []tokenauth.Descriptor {
	plans := c.ProviderTokenSources()
	indexByHost := make(map[string]int, len(plans))
	chainByHost := make(map[string]string, len(plans))
	out := make([]tokenauth.Descriptor, 0, len(plans))
	for _, plan := range plans {
		host := plan.Descriptor.Key.Host
		idx, ok := indexByHost[host]
		if !ok {
			indexByHost[host] = len(out)
			out = append(out, tokenauth.Descriptor{Key: tokenauth.CloneKey(host)})
			idx = len(out) - 1
		}
		// Managed Git selects scoped GitHub routes by repository identity;
		// only unscoped provider chains participate in the host fallback.
		if plan.Descriptor.Key.Platform == defaultPlatform &&
			plan.Descriptor.Key.Scope != "" {
			continue
		}
		if len(plan.Descriptor.Candidates) == 0 {
			continue
		}
		chain := plan.Descriptor.CanonicalSourceString()
		existing, seen := chainByHost[host]
		if !seen {
			chainByHost[host] = chain
			out[idx].Candidates = plan.Descriptor.Candidates
			continue
		}
		if existing != chain {
			out[idx].Candidates = nil
		}
	}
	return out
}

// ValidateRepoTokenSourceConsistency requires repositories sharing one
// non-GitHub (provider, host) to declare the same effective token chain,
// because those providers resolve API and clone credentials per provider-host
// pair. The check must use each repository's own descriptor:
// ProviderTokenSources deduplicates by key and keeps only the first same-host
// plan, which would hide the conflict. Distinct providers sharing a hostname
// may carry different chains; ownerless host operations lose their fallback
// in that case (see CloneTokenDescriptors) instead of failing validation.
func (c *Config) ValidateRepoTokenSourceConsistency() error {
	if c == nil {
		return nil
	}
	repoSources := make(map[tokenauth.Key]tokenauth.Descriptor, len(c.Repos))
	for _, r := range c.Repos {
		if r.PlatformOrDefault() == defaultPlatform {
			continue
		}
		effective := c.ResolveRepoTokenSource(r)
		if effective.Key.Host == "" {
			continue
		}
		prev, ok := repoSources[effective.Key]
		if !ok {
			repoSources[effective.Key] = effective
			continue
		}
		if prev.EqualSource(effective) {
			continue
		}
		return fmt.Errorf(
			"conflicting token source for %s host %q (conflicting token_env): %s vs %s",
			r.PlatformOrDefault(), r.PlatformHostOrDefault(),
			prev.SafeString(), effective.SafeString(),
		)
	}
	return nil
}

func (c *Config) TokenSourceForPlatformHost(
	platform, host, repoTokenEnv, repoTokenFile string,
) tokenauth.Descriptor {
	if c == nil {
		return tokenauth.Descriptor{}
	}
	p, err := normalizePlatform(platform)
	if err != nil {
		return tokenauth.Descriptor{}
	}
	h, err := normalizePlatformHost(p, host)
	if err != nil {
		return tokenauth.Descriptor{}
	}
	desc := tokenauth.Descriptor{Key: tokenauth.Key{Platform: p, Host: h}}
	appendTokenFileEnvCandidates(&desc, repoTokenFile, repoTokenEnv)
	// GitHub App installations are account-scoped and therefore belong only
	// on repository or owner routes built by ResolveGitHubRepoTokenSource.
	// An ownerless host fallback must remain PAT/gh-only so its authenticated
	// credential and identity-scoped rate accounting cannot disagree.
	c.appendPlatformTokenCandidates(&desc, p, h)
	if defaultTokenEnv, ok := defaultTokenEnvForPlatformHost(p, h); ok {
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind:    tokenauth.SourceKindEnv,
			EnvName: defaultTokenEnv,
		})
	}
	if p == defaultPlatform {
		// github_token_env is a github.com-only default, mirroring the
		// other public-host defaults. Appending it for Enterprise or
		// self-hosted GitHub hosts would send the public-GitHub token to
		// whatever host the config names; those hosts must configure
		// repo/platform token_env or token_file, or rely on gh's
		// host-scoped credential below.
		if c.GitHubTokenEnv != "" && h == platformpkg.DefaultGitHubHost {
			desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
				Kind:    tokenauth.SourceKindEnv,
				EnvName: c.GitHubTokenEnv,
			})
		}
		desc.Candidates = append(desc.Candidates, tokenauth.Candidate{
			Kind: tokenauth.SourceKindGitHubCLI,
			Host: h,
		})
	}
	return desc
}

func defaultTokenEnvForPlatformHost(platform, host string) (string, bool) {
	switch platform {
	case string(platformpkg.KindForgejo):
		return defaultForgejoTokenEnv, host == platformpkg.DefaultForgejoHost
	case string(platformpkg.KindGitea):
		return defaultGiteaTokenEnv, host == platformpkg.DefaultGiteaHost
	default:
		return "", false
	}
}

// TokenEnvNames returns every env var name that may hold a provider
// token according to this config. Used by the runtime sanitizer to
// strip tokens from launched session environments.
func (c *Config) TokenEnvNames() []string {
	if c == nil {
		return nil
	}
	names := make(
		[]string, 0, 1+len(c.Repos)+len(c.Platforms)+len(c.GitHubOwnerTokens),
	)
	if c.GitHubTokenEnv != "" {
		names = appendTokenEnvName(names, c.GitHubTokenEnv)
	}
	for _, item := range c.GitHubOwnerTokens {
		names = appendTokenEnvName(names, item.TokenEnv)
	}
	for _, p := range c.Platforms {
		names = appendTokenEnvNamesFromDescriptor(
			names,
			c.TokenSourceForPlatformHost(p.Type, p.Host, "", ""),
		)
	}
	for _, r := range c.Repos {
		names = appendTokenEnvNamesFromDescriptor(
			names,
			c.TokenSourceForPlatformHost(
				r.PlatformOrDefault(),
				r.PlatformHostOrDefault(),
				"",
				"",
			),
		)
	}
	for _, r := range c.Repos {
		names = appendTokenEnvNamesFromDescriptor(names, c.ResolveRepoTokenSource(r))
	}
	// Explicitly declared names are collected verbatim as well:
	// descriptor resolution returns nothing for invalid provider
	// identities, and a rejected candidate's declared credential names
	// must still reach strip accumulation and collision validation.
	for _, p := range c.Platforms {
		names = appendTokenEnvName(names, strings.TrimSpace(p.TokenEnv))
	}
	for _, r := range c.Repos {
		names = appendTokenEnvName(names, strings.TrimSpace(r.TokenEnv))
	}
	return names
}

func appendTokenEnvNamesFromDescriptor(
	names []string,
	desc tokenauth.Descriptor,
) []string {
	for _, candidate := range desc.Candidates {
		if candidate.Kind == tokenauth.SourceKindEnv {
			names = appendTokenEnvName(names, candidate.EnvName)
		}
	}
	return names
}

func (c *Config) normalizeTokenFilePaths(configDir string) {
	for i := range c.GitHubOwnerTokens {
		c.GitHubOwnerTokens[i].TokenFile = normalizeTokenFilePath(
			configDir, c.GitHubOwnerTokens[i].TokenFile,
		)
	}
	for i := range c.Platforms {
		c.Platforms[i].TokenFile = normalizeTokenFilePath(configDir, c.Platforms[i].TokenFile)
	}
	for i := range c.Repos {
		c.Repos[i].TokenFile = normalizeTokenFilePath(configDir, c.Repos[i].TokenFile)
	}
	for i := range c.GitHubApps {
		c.GitHubApps[i].PrivateKeyPath = normalizeTokenFilePath(
			configDir, c.GitHubApps[i].PrivateKeyPath,
		)
	}
}

func normalizeTokenFilePath(configDir, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == "~" {
		return homeDir()
	}
	if suffix, ok := strings.CutPrefix(raw, "~/"); ok {
		return filepath.Join(homeDir(), suffix)
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(configDir, raw))
}

func appendTokenEnvName(names []string, name string) []string {
	if name == "" || slices.Contains(names, name) {
		return names
	}
	return append(names, name)
}

var execCommand = procutil.CommandContext

// ghAuthExecTimeout bounds each gh subprocess invocation. gh auth
// token is a local lookup and returns in milliseconds; 5s is generous
// and prevents a hung gh from stalling startup. A var rather than a
// const only so tests driving fake gh scripts can relax it: under a
// fully loaded parallel suite run, spawning the fake can exceed 5s and
// the kill then masquerades as gh behavior.
var ghAuthExecTimeout = 5 * time.Second

// ghAuthTokenForHost returns the token gh has stored for host, or "".
// Older gh versions that do not recognize --hostname trigger a fallback
// to bare `gh auth token` only when host is the default github.com.
// Any other host returns empty without retry so the caller surfaces a
// missing-token error rather than the wrong host's token.
func ghAuthTokenForHost(host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), ghAuthExecTimeout)
	defer cancel()
	token, _ := GitHubCLITokenForHost(ctx, host)
	return token
}

func GitHubCLITokenForHost(ctx context.Context, host string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ghAuthExecTimeout)
		defer cancel()
	}
	out, stderr, err := runGHAuthToken(ctx, "--hostname", host)
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if host == platformpkg.DefaultGitHubHost &&
		isUnsupportedHostnameFlag(err, stderr) {
		out, _, err = runGHAuthToken(ctx)
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", nil
}

// runGHAuthToken invokes `gh auth token` with the given extra args
// under ctx. stderr is captured explicitly so the caller can inspect
// the rejection text from older gh versions (cmd.Output() only fills
// *ExitError.Stderr when cmd.Stderr is unset).
func runGHAuthToken(ctx context.Context, extraArgs ...string) ([]byte, []byte, error) {
	args := append([]string{"auth", "token"}, extraArgs...)
	cmd := execCommand(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.Bytes(), err
}

// isUnsupportedHostnameFlag reports whether the gh invocation failed
// specifically because the installed gh does not recognize the
// --hostname flag (cobra/pflag rejection text). Missing-binary,
// context-deadline, auth-failure, and unrelated nonzero exits all
// return false so the caller does not retry bare.
func isUnsupportedHostnameFlag(err error, stderr []byte) bool {
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		return false
	}
	text := string(stderr)
	return strings.Contains(text, "unknown flag: --hostname") ||
		strings.Contains(text, "unknown shorthand flag")
}

func (c *Config) BudgetPerHour() int {
	return c.SyncBudgetPerHour
}

func (c *Config) ListenAddr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// BindHostKey returns the canonical (Host, Port) key for the bind
// address, populated by Validate. The zero HostKey is returned for
// configs that were not validated (e.g. test literals that omit
// Host/Port); callers should use HostKey.Valid() to gate behavior.
func (c *Config) BindHostKey() HostKey {
	return c.parsedBindKey
}

// ParsedAllowedHosts returns the canonicalised allowlist, populated
// by Validate. The returned slice is a defensive copy.
func (c *Config) ParsedAllowedHosts() []HostKey {
	return append([]HostKey(nil), c.parsedAllowedHosts...)
}

func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "forge.db")
}

// RoborevEndpoint returns the configured roborev daemon endpoint,
// falling back to the default localhost address.
func (c *Config) RoborevEndpoint() string {
	if c.Roborev.Endpoint != "" {
		return c.Roborev.Endpoint
	}
	return "http://127.0.0.1:7373"
}

// DefaultTmuxCommand returns the command + argv prefix used to invoke
// tmux when no [tmux] command is configured. The -L socket isolates
// kenn-forge sessions on a dedicated tmux server so heavy Forge
// activity does not contend with the user's global tmux server. The
// returned slice is a fresh copy, safe to append to.
func DefaultTmuxCommand() []string {
	return []string{"tmux", "-L", "kenn-forge"}
}

// IsDefaultTmuxCommand reports whether command selects Forge's dedicated tmux
// server. An empty command uses that default through TmuxCommand.
func IsDefaultTmuxCommand(command []string) bool {
	return len(command) == 0 || slices.Equal(command, DefaultTmuxCommand())
}

// tmuxNonSecretEnvVars names every variable admitted into tmux client
// environments and, transitively, the tmux server's permanently
// retained spawn environment — exact names only, never prefixes, which
// a secret could hide under. Because these values are deliberately
// non-secret, config validation rejects token env names that collide
// with them: a running tmux server retains its spawn environment, so a
// name on this list could never be scrubbed once declared secret.
var tmuxNonSecretEnvVars = []string{
	"COLORTERM",
	"EDITOR",
	"HOME",
	// Locale: LANG/LANGUAGE plus the POSIX and glibc LC_* categories.
	"LANG",
	"LANGUAGE",
	"LC_ADDRESS",
	"LC_ALL",
	"LC_COLLATE",
	"LC_CTYPE",
	"LC_IDENTIFICATION",
	"LC_MEASUREMENT",
	"LC_MESSAGES",
	"LC_MONETARY",
	"LC_NAME",
	"LC_NUMERIC",
	"LC_PAPER",
	"LC_TELEPHONE",
	"LC_TIME",
	"LESS",
	"LOGNAME",
	"NO_COLOR",
	"PAGER",
	"PATH",
	"SHELL",
	"SSH_AUTH_SOCK",
	"TERM",
	"TMP",
	"TMPDIR",
	// tmux resolves -L sockets under TMUX_TMPDIR; dropping it would
	// route the tmux client to a different server than the manager
	// owns.
	"TMUX_TMPDIR",
	"TEMP",
	"USER",
	"VISUAL",
	// XDG base directories and session identity.
	"XDG_CACHE_HOME",
	"XDG_CONFIG_DIRS",
	"XDG_CONFIG_HOME",
	"XDG_CURRENT_DESKTOP",
	"XDG_DATA_DIRS",
	"XDG_DATA_HOME",
	"XDG_RUNTIME_DIR",
	"XDG_SEAT",
	"XDG_SESSION_CLASS",
	"XDG_SESSION_DESKTOP",
	"XDG_SESSION_ID",
	"XDG_SESSION_TYPE",
	"XDG_STATE_HOME",
	"XDG_VTNR",
}

// IsTmuxNonSecretEnvVar reports whether name belongs to the non-secret
// environment contract for tmux clients and terminal sessions. The
// comparison is case-insensitive on every platform because this
// predicate REJECTS: token names may never collide with a reserved name
// in any casing (Windows resolves env names case-insensitively), and
// over-rejection is safe. Environment ADMISSION must not use this
// predicate on case-sensitive platforms — use
// IsTmuxNonSecretEnvVarExact there, or a Unix variable literally named
// "editor" would be admitted into tmux's retained environment.
func IsTmuxNonSecretEnvVar(name string) bool {
	return slices.ContainsFunc(tmuxNonSecretEnvVars, func(v string) bool {
		return strings.EqualFold(v, name)
	})
}

// IsTmuxNonSecretEnvVarExact is the exact-case admission predicate for
// case-sensitive platforms.
func IsTmuxNonSecretEnvVarExact(name string) bool {
	return slices.Contains(tmuxNonSecretEnvVars, name)
}

// TmuxCommand returns the command + argv prefix used to invoke tmux.
// Defaults to DefaultTmuxCommand when c is nil or the setting is
// unconfigured. An explicitly configured command is returned verbatim, but
// still identifies the tmux server Forge owns and configures. The returned
// slice is a copy, safe to append to.
func (c *Config) TmuxCommand() []string {
	if c == nil || len(c.Tmux.Command) == 0 {
		return DefaultTmuxCommand()
	}
	return slices.Clone(c.Tmux.Command)
}

// ShellCommand returns the configured shell command + argv prefix
// used when ensuring a workspace's plain shell session, or nil when
// unset. nil means the runtime falls back to the user's $SHELL (or
// /bin/sh). The returned slice is a copy, safe to append to.
func (c *Config) ShellCommand() []string {
	if c == nil || len(c.Shell.Command) == 0 {
		return nil
	}
	return slices.Clone(c.Shell.Command)
}

// TerminalTmuxMouseEnabled reports whether Forge-managed tmux sessions should
// enable mouse handling. It defaults to true for omitted and nil configs.
func (c *Config) TerminalTmuxMouseEnabled() bool {
	return c == nil || c.Terminal.TmuxMouse == nil || *c.Terminal.TmuxMouse
}

// TerminalGraphicsEnabled reports whether Forge-managed terminals should
// decode images and configure tmux graphics support. It defaults to true for
// omitted and nil configs.
func (c *Config) TerminalGraphicsEnabled() bool {
	return c == nil || c.Terminal.Graphics == nil || *c.Terminal.Graphics
}

// TmuxAgentSessionsEnabled reports whether runtime agent launches
// should prefer tmux-backed sessions. Defaults to true so agent
// activity is visible to tmux-based workspace fingerprinting.
func (c *Config) TmuxAgentSessionsEnabled() bool {
	return c == nil ||
		c.Tmux.AgentSessions == nil ||
		*c.Tmux.AgentSessions
}

// IssueWorkspaceBranchSlugEnabled reports whether new issue
// workspaces should derive a title slug onto their branch name.
// Defaults to true (the "slug" style); returns false for "bare".
func (c *Config) IssueWorkspaceBranchSlugEnabled() bool {
	if c == nil {
		return true
	}
	switch strings.TrimSpace(c.IssueWorkspaceBranchStyle) {
	case "", IssueWorkspaceBranchStyleSlug:
		return true
	case IssueWorkspaceBranchStyleBare:
		return false
	default:
		return true
	}
}

func reposForSave(repos []Repo) []Repo {
	if repos == nil {
		return nil
	}
	out := make([]Repo, len(repos))
	copy(out, repos)
	for i := range out {
		if out[i].Platform == defaultPlatform {
			out[i].Platform = ""
		}
		if out[i].PlatformOrDefault() == defaultPlatform &&
			out[i].PlatformHost == defaultPlatformHost {
			out[i].PlatformHost = ""
		}
	}
	return out
}

// configFile is the subset of Config written to disk.
type configFile struct {
	SyncInterval              string                   `toml:"sync_interval"`
	ActivePRRefreshInterval   string                   `toml:"active_pr_refresh_interval"`
	ActivePRWindow            string                   `toml:"active_pr_window"`
	GitHubTokenEnv            string                   `toml:"github_token_env"`
	DefaultPlatformHost       string                   `toml:"default_platform_host,omitempty"`
	Host                      string                   `toml:"host"`
	Port                      int                      `toml:"port"`
	SyncBudgetPerHour         int                      `toml:"sync_budget_per_hour,omitempty"`
	SSEBufferSize             int                      `toml:"sse_buffer_size,omitempty"`
	BasePath                  string                   `toml:"base_path,omitempty"`
	DataDir                   string                   `toml:"data_dir,omitempty"`
	IssueWorkspaceBranchStyle string                   `toml:"issue_workspace_branch_style,omitempty"`
	AllowedHosts              []string                 `toml:"allowed_hosts,omitempty"`
	TrustReverseProxy         bool                     `toml:"trust_reverse_proxy,omitempty"`
	Repos                     []Repo                   `toml:"repos"`
	RepoPresets               []RepoPreset             `toml:"repo_presets,omitempty"`
	KataProjects              []KataProjectRepoMapping `toml:"kata_projects,omitempty"`
	Platforms                 []PlatformConfig         `toml:"platforms,omitempty"`
	GitHubOwnerTokens         []GitHubOwnerTokenConfig `toml:"github_owner_tokens,omitempty"`
	GitHubApps                []GitHubAppConfig        `toml:"github_apps,omitempty"`
	Activity                  Activity                 `toml:"activity"`
	Notifications             Notifications            `toml:"notifications,omitempty"`
	Terminal                  Terminal                 `toml:"terminal,omitempty"`
	Modes                     ModeVisibility           `toml:"modes,omitempty"`
	Agents                    []Agent                  `toml:"agents,omitempty"`
	DocFolders                []DocFolder              `toml:"doc_folders,omitempty"`
	Roborev                   Roborev                  `toml:"roborev,omitempty"`
	PullRequests              PullRequests             `toml:"pull_requests,omitempty"`
	Detail                    Detail                   `toml:"detail,omitempty"`
	Workspaces                Workspaces               `toml:"workspaces,omitempty"`
	Issues                    Issues                   `toml:"issues,omitempty"`
	Tmux                      Tmux                     `toml:"tmux,omitempty"`
	Shell                     Shell                    `toml:"shell,omitempty"`
	Fleet                     Fleet                    `toml:"fleet,omitempty"`
	API                       API                      `toml:"api,omitempty"`
	MCP                       MCP                      `toml:"mcp,omitempty"`
}

// Save writes the current config to the given path.
func (c *Config) Save(path string) error {
	cfg := c.copyForSave()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}
	f := configFile{
		SyncInterval:            cfg.SyncInterval,
		ActivePRRefreshInterval: cfg.ActivePRRefreshInterval,
		ActivePRWindow:          cfg.ActivePRWindow,
		GitHubTokenEnv:          cfg.GitHubTokenEnv,
		DefaultPlatformHost:     cfg.DefaultPlatformHost,
		Host:                    cfg.Host,
		Port:                    cfg.Port,
		AllowedHosts:            slices.Clone(cfg.AllowedHosts),
		TrustReverseProxy:       cfg.TrustReverseProxy,
		Repos:                   reposForSave(cfg.Repos),
		RepoPresets:             cloneRepoPresets(cfg.RepoPresets),
		KataProjects:            slices.Clone(cfg.KataProjects),
		Platforms:               cfg.Platforms,
		GitHubOwnerTokens:       cfg.GitHubOwnerTokens,
		GitHubApps:              cfg.GitHubApps,
		Activity:                cfg.Activity,
		Notifications:           cfg.Notifications,
		Terminal:                cfg.Terminal,
		Modes:                   cfg.Modes,
		Agents:                  cfg.Agents,
		DocFolders:              cfg.DocFolders,
		Roborev:                 cfg.Roborev,
		PullRequests:            cfg.PullRequests,
		Detail:                  cfg.Detail,
		Workspaces:              cfg.Workspaces,
		Issues:                  cfg.Issues,
		Tmux:                    cfg.Tmux,
		Shell:                   cfg.Shell,
		Fleet:                   cfg.Fleet,
		API:                     cfg.API,
		MCP:                     cfg.MCP,
	}
	if cfg.DefaultPlatformHost == defaultPlatformHost {
		f.DefaultPlatformHost = ""
	}
	if cfg.SyncBudgetPerHour != defaultSyncBudgetPerHour {
		f.SyncBudgetPerHour = cfg.SyncBudgetPerHour
	}
	if cfg.SSEBufferSize != 0 && cfg.SSEBufferSize != defaultSSEBufferSize {
		f.SSEBufferSize = cfg.SSEBufferSize
	}
	if cfg.BasePath != defaultBasePath {
		f.BasePath = cfg.BasePath
	}
	if cfg.DataDir != DefaultDataDir() {
		f.DataDir = cfg.DataDir
	}
	if cfg.IssueWorkspaceBranchStyle != defaultIssueWorkspaceBranchStyle {
		f.IssueWorkspaceBranchStyle = cfg.IssueWorkspaceBranchStyle
	}

	savePath := path
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolving config symlink: %w", err)
		}
		savePath = resolved
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checking config path: %w", err)
	}

	dir := filepath.Dir(savePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating config directory: %w", err)
		}
	}

	tmp, err := os.CreateTemp(dir, ".kenn-forge-config-*.toml")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(f); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	if err := os.Rename(tmpPath, savePath); err != nil {
		return fmt.Errorf("renaming temp config: %w", err)
	}
	return nil
}

func (c *Config) copyForSave() Config {
	if c == nil {
		return Config{}
	}
	cfg := *c
	cfg.Repos = slices.Clone(c.Repos)
	cfg.RepoPresets = cloneRepoPresets(c.RepoPresets)
	cfg.KataProjects = slices.Clone(c.KataProjects)
	cfg.Platforms = slices.Clone(c.Platforms)
	cfg.AllowedHosts = slices.Clone(c.AllowedHosts)
	cfg.GitHubOwnerTokens = slices.Clone(c.GitHubOwnerTokens)
	cfg.GitHubApps = slices.Clone(c.GitHubApps)
	for i := range cfg.GitHubApps {
		cfg.GitHubApps[i].SelectedRepos = slices.Clone(cfg.GitHubApps[i].SelectedRepos)
	}
	cfg.DocFolders = slices.Clone(c.DocFolders)
	cfg.Agents = slices.Clone(c.Agents)
	cfg.API.TailscaleServe.AllowedUsers = slices.Clone(c.API.TailscaleServe.AllowedUsers)
	if c.Fleet.Hub != nil {
		hub := *c.Fleet.Hub
		cfg.Fleet.Hub = &hub
	}
	cfg.Fleet.Members = slices.Clone(c.Fleet.Members)
	if cfg.SyncInterval == "" {
		cfg.SyncInterval = defaultSyncInterval
	}
	if cfg.ActivePRRefreshInterval == "" {
		cfg.ActivePRRefreshInterval = defaultActivePRRefreshInterval
	}
	if cfg.ActivePRWindow == "" {
		cfg.ActivePRWindow = defaultActivePRWindow
	}
	if cfg.DefaultPlatformHost == "" {
		cfg.DefaultPlatformHost = defaultPlatformHost
	}
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	cfg.Modes = cfg.Modes.WithDefaults()
	if cfg.Activity.ViewMode == "" {
		cfg.Activity.ViewMode = defaultViewMode
	}
	if cfg.Activity.TimeRange == "" {
		cfg.Activity.TimeRange = defaultTimeRange
	}
	if cfg.Activity.DefaultBranchRetentionDays == 0 {
		cfg.Activity.DefaultBranchRetentionDays = defaultBranchActivityRetentionDays
	}
	if cfg.Activity.DefaultBranchMaxCommits == 0 {
		cfg.Activity.DefaultBranchMaxCommits = defaultBranchActivityMaxCommits
	}
	if cfg.SyncBudgetPerHour == 0 {
		cfg.SyncBudgetPerHour = defaultSyncBudgetPerHour
	}
	if cfg.SSEBufferSize == 0 {
		cfg.SSEBufferSize = defaultSSEBufferSize
	}
	if cfg.BasePath == "" {
		cfg.BasePath = defaultBasePath
	}
	if strings.TrimSpace(cfg.IssueWorkspaceBranchStyle) == "" {
		cfg.IssueWorkspaceBranchStyle = defaultIssueWorkspaceBranchStyle
	}
	return cfg
}
