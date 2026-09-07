package github

import (
	"context"
	"fmt"
	"path"
	"strings"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/platform"
)

func canonicalRepoName(name string) string {
	return strings.ToLower(name)
}

func canonicalRepoOwner(owner string) string {
	return strings.ToLower(owner)
}

func canonicalRepoHost(host string) string {
	if host == "" {
		host = "github.com"
	}
	return strings.ToLower(host)
}

func canonicalRepoRef(repo RepoRef) RepoRef {
	kind := repoPlatform(repo)
	out := RepoRef{
		Platform:           kind,
		Owner:              strings.TrimSpace(repo.Owner),
		Name:               strings.TrimSpace(repo.Name),
		PlatformHost:       canonicalRepoHost(repo.PlatformHost),
		RepoPath:           strings.TrimSpace(repo.RepoPath),
		PlatformRepoID:     repo.PlatformRepoID,
		PlatformExternalID: strings.TrimSpace(repo.PlatformExternalID),
		WebURL:             strings.TrimSpace(repo.WebURL),
		CloneURL:           strings.TrimSpace(repo.CloneURL),
		DefaultBranch:      strings.TrimSpace(repo.DefaultBranch),
		Archived:           repo.Archived,
		ConfiguredRepoPath: strings.TrimSpace(repo.ConfiguredRepoPath),
	}
	if kind == platform.KindGitHub {
		out.Owner = canonicalRepoOwner(out.Owner)
		out.Name = canonicalRepoName(out.Name)
		if out.RepoPath != "" {
			out.RepoPath = canonicalRepoName(out.RepoPath)
		}
	}
	if out.RepoPath == "" {
		out.RepoPath = out.Owner + "/" + out.Name
	}
	return out
}

func canonicalRepoPattern(pattern string) string {
	return strings.ToLower(pattern)
}

type ConfiguredRepoStatus struct {
	Provider       string `json:"provider"`
	PlatformHost   string `json:"platform_host"`
	PlatformRepoID string `json:"platform_repo_id,omitempty"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	RepoPath       string `json:"repo_path"`
	// TrackedRepoPath is the provider-verified current route of the tracked
	// repository backing an exact entry. After a provider-side rename it
	// differs from the configured RepoPath, and clients need it to release
	// state keyed to the current route (for example filter selections of a
	// repository being hidden). Empty for globs and untracked entries.
	TrackedRepoPath   string `json:"tracked_repo_path,omitempty"`
	WorktreeBasePath  string `json:"worktree_base_path,omitempty"`
	IsGlob            bool   `json:"is_glob"`
	MatchedRepoCount  int    `json:"matched_repo_count"`
	HiddenFromUI      bool   `json:"hidden_from_ui"`
	IssuePRReferences bool   `json:"issue_pr_references"`
}

type ResolveConfiguredReposResult struct {
	Configured []ConfiguredRepoStatus
	Expanded   []RepoRef
	Warnings   []error
}

func FallbackConfiguredRepoRefs(
	previous []RepoRef,
	raw config.Repo,
) []RepoRef {
	kind := platform.Kind(raw.PlatformOrDefault())
	host := raw.PlatformHostOrDefault()
	repoPath := configuredRepoPath(raw)
	if !raw.HasNameGlob() {
		// Provenance first: a provider-side rename moves the tracked route
		// away from the configured path, but the tracked ref still records
		// which config entry it was resolved from. Falling back to it keeps
		// the provider identity and archived state instead of synthesizing
		// an identity-less live duplicate under the stale route.
		for _, repo := range previous {
			if repoPlatform(repo) == kind &&
				sameConfiguredRepoHost(repoHost(repo), host) &&
				strings.EqualFold(repo.ConfiguredRepoPath, repoPath) {
				return []RepoRef{repo}
			}
		}
		for _, repo := range previous {
			if repoPlatform(repo) == kind &&
				sameConfiguredRepoHost(repoHost(repo), host) &&
				strings.EqualFold(repoPathOrFullName(repo), repoPath) {
				return []RepoRef{repo}
			}
		}
		return []RepoRef{fallbackRepoRef(raw, kind, host)}
	}

	fallback := make([]RepoRef, 0)
	for _, repo := range previous {
		if repoPlatform(repo) != kind ||
			!sameConfiguredRepoHost(repoHost(repo), host) ||
			!strings.EqualFold(repo.Owner, raw.Owner) {
			continue
		}
		matched, err := path.Match(
			canonicalRepoPattern(raw.Name),
			canonicalRepoName(repo.Name),
		)
		if err != nil || !matched {
			continue
		}
		fallback = append(fallback, repo)
	}
	return fallback
}

func fallbackRepoRef(raw config.Repo, kind platform.Kind, host string) RepoRef {
	repo := RepoRef{
		Platform:           kind,
		Owner:              strings.TrimSpace(raw.Owner),
		Name:               strings.TrimSpace(raw.Name),
		PlatformHost:       strings.ToLower(strings.TrimSpace(host)),
		RepoPath:           strings.TrimSpace(configuredRepoPath(raw)),
		ConfiguredRepoPath: configuredRepoPath(raw),
	}
	if kind == "" {
		kind = platform.KindGitHub
	}
	if kind == platform.KindGitHub {
		repo.Owner = canonicalRepoOwner(repo.Owner)
		repo.Name = canonicalRepoName(repo.Name)
		repo.PlatformHost = canonicalRepoHost(repo.PlatformHost)
	}
	return repo
}

func ResolveConfiguredRepos(
	ctx context.Context,
	clients map[string]Client,
	repos []config.Repo,
) ResolveConfiguredReposResult {
	return resolveConfiguredRepos(ctx, registryFromGitHubClients(clients), repos)
}

func ResolveConfiguredReposWithRegistry(
	ctx context.Context,
	registry *platform.Registry,
	repos []config.Repo,
) ResolveConfiguredReposResult {
	return resolveConfiguredRepos(ctx, registry, repos)
}

func resolveConfiguredRepos(
	ctx context.Context,
	registry *platform.Registry,
	repos []config.Repo,
) ResolveConfiguredReposResult {
	seen := make(map[string]struct{})
	result := ResolveConfiguredReposResult{
		Configured: make([]ConfiguredRepoStatus, 0, len(repos)),
	}

	for _, raw := range repos {
		status, expanded, err := resolveConfiguredRepo(
			ctx, registry, raw,
		)
		if err != nil {
			status.MatchedRepoCount = 0
			result.Warnings = append(result.Warnings, err)
		}
		result.Configured = append(result.Configured, status)
		for _, repo := range expanded {
			appendExpandedRepo(&result.Expanded, seen, repo)
		}
	}

	return result
}

func ResolveConfiguredRepo(
	ctx context.Context,
	clients map[string]Client,
	repo config.Repo,
) (ConfiguredRepoStatus, []RepoRef, error) {
	return resolveConfiguredRepo(ctx, registryFromGitHubClients(clients), repo)
}

func ResolveConfiguredRepoWithRegistry(
	ctx context.Context,
	registry *platform.Registry,
	repo config.Repo,
) (ConfiguredRepoStatus, []RepoRef, error) {
	return resolveConfiguredRepo(ctx, registry, repo)
}

func resolveConfiguredRepo(
	ctx context.Context,
	registry *platform.Registry,
	raw config.Repo,
) (ConfiguredRepoStatus, []RepoRef, error) {
	status := ConfiguredRepoStatus{
		Provider:     raw.PlatformOrDefault(),
		PlatformHost: raw.PlatformHostOrDefault(),
		Owner:        raw.Owner,
		Name:         raw.Name,
		RepoPath:     configuredRepoPath(raw),
		IsGlob:       raw.HasNameGlob(),
	}
	kind := platform.Kind(raw.PlatformOrDefault())
	host := raw.PlatformHostOrDefault()
	repoPath := configuredRepoPath(raw)
	reader, err := registry.RepositoryReader(kind, host)
	if err != nil {
		return status, nil, err
	}

	if !status.IsGlob {
		repo, err := reader.GetRepository(ctx, platform.RepoRef{
			Platform: kind,
			Host:     host,
			Owner:    raw.Owner,
			Name:     raw.Name,
			RepoPath: repoPath,
		})
		if err != nil {
			return status, nil, fmt.Errorf(
				"resolve configured repo %s/%s: %w",
				raw.Owner, raw.Name, err,
			)
		}
		resolved := repoRefFromRepository(raw, kind, host, repo)
		configuredProviderID := strings.TrimSpace(raw.PlatformRepoID)
		if configuredProviderID != "" &&
			resolved.PlatformExternalID != configuredProviderID {
			return status, nil, fmt.Errorf(
				"resolve configured repo %s/%s: provider repository ID changed",
				raw.Owner, raw.Name,
			)
		}
		status.MatchedRepoCount = 1
		status.PlatformRepoID = resolved.PlatformExternalID
		return status, []RepoRef{resolved}, nil
	}

	repos, err := reader.ListRepositories(ctx, raw.Owner, platform.RepositoryListOptions{
		IncludeArchived: true,
	})
	if err != nil {
		return status, nil, fmt.Errorf(
			"resolve configured repo glob %s/%s: %w",
			raw.Owner, raw.Name, err,
		)
	}

	matches := make([]RepoRef, 0, len(repos))
	for _, repo := range repos {
		repoName := repo.Ref.Name
		if repoName == "" {
			repoName = repo.Ref.DisplayName()
		}
		matched, err := path.Match(
			canonicalRepoName(raw.Name),
			canonicalRepoName(repoName),
		)
		if err != nil {
			return status, nil, fmt.Errorf(
				"invalid repo glob %s/%s: %w",
				raw.Owner, raw.Name, err,
			)
		}
		if !matched {
			continue
		}
		matches = append(matches, repoRefFromRepository(raw, kind, host, repo))
	}
	status.MatchedRepoCount = len(matches)
	return status, matches, nil
}

func configuredRepoPath(raw config.Repo) string {
	if strings.TrimSpace(raw.RepoPath) != "" {
		return strings.TrimSpace(raw.RepoPath)
	}
	return raw.Owner + "/" + raw.Name
}

// exactConfiguredRepoPath is the provenance stamped on resolved refs: only
// exact entries author it — a glob pattern identifies no single entry to
// correlate with, and stamping it would displace exact provenance on
// deduplicated overlaps.
func exactConfiguredRepoPath(raw config.Repo) string {
	if raw.HasNameGlob() {
		return ""
	}
	return configuredRepoPath(raw)
}

func repoPathOrFullName(repo RepoRef) string {
	if strings.TrimSpace(repo.RepoPath) != "" {
		return strings.TrimSpace(repo.RepoPath)
	}
	return repo.Owner + "/" + repo.Name
}

func repoRefFromRepository(
	raw config.Repo,
	kind platform.Kind,
	host string,
	repo platform.Repository,
) RepoRef {
	owner := repo.Ref.Owner
	if owner == "" {
		owner = raw.Owner
	}
	name := repo.Ref.Name
	if name == "" {
		name = raw.Name
	}
	ref := RepoRef{
		Platform:           kind,
		Owner:              strings.TrimSpace(owner),
		Name:               strings.TrimSpace(name),
		PlatformHost:       canonicalRepoHost(host),
		RepoPath:           strings.TrimSpace(repo.Ref.RepoPath),
		PlatformRepoID:     repo.PlatformID,
		PlatformExternalID: repo.PlatformExternalID,
		WebURL:             repo.WebURL,
		CloneURL:           repo.CloneURL,
		DefaultBranch:      repo.DefaultBranch,
		Archived:           repo.Archived,
		ConfiguredRepoPath: exactConfiguredRepoPath(raw),
	}
	if ref.PlatformRepoID == 0 {
		ref.PlatformRepoID = repo.Ref.PlatformID
	}
	if ref.PlatformExternalID == "" {
		ref.PlatformExternalID = repo.Ref.PlatformExternalID
	}
	if ref.WebURL == "" {
		ref.WebURL = repo.Ref.WebURL
	}
	if ref.CloneURL == "" {
		ref.CloneURL = repo.Ref.CloneURL
	}
	if ref.DefaultBranch == "" {
		ref.DefaultBranch = repo.Ref.DefaultBranch
	}
	if kind == platform.KindGitHub {
		ref.Owner = canonicalRepoOwner(ref.Owner)
		ref.Name = canonicalRepoName(ref.Name)
		ref.RepoPath = canonicalRepoName(ref.RepoPath)
	}
	if ref.RepoPath == "" {
		ref.RepoPath = ref.Owner + "/" + ref.Name
	}
	return ref
}

func appendExpandedRepo(
	dst *[]RepoRef,
	seen map[string]struct{},
	repo RepoRef,
) {
	repo = canonicalRepoRef(repo)
	key := string(repoPlatform(repo)) + "\x00" + repo.PlatformHost + "\x00" + repo.Owner + "\x00" + repo.Name
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*dst = append(*dst, repo)
}

// ExpandedRepoSet accumulates configured-repo expansions across config
// entries, deduplicating by platform/host/owner/name and — when a stable
// provider id is present — by platform/host/provider-id, so a renamed route
// cannot track the same repository twice. A provider-resolved ref replaces a
// fallback-derived duplicate from an earlier entry, so a transient resolve
// failure on one entry cannot freeze stale metadata (an archived flip, a
// rename) when an overlapping entry resolved successfully. Fallback refs
// never overwrite resolved ones; same-class duplicates keep the first entry.
type ExpandedRepoSet struct {
	refs       []RepoRef
	resolved   []bool
	byRoute    map[string]int
	byIdentity map[string]int
}

func NewExpandedRepoSet() *ExpandedRepoSet {
	return &ExpandedRepoSet{
		byRoute:    make(map[string]int),
		byIdentity: make(map[string]int),
	}
}

func expandedRepoRouteKey(repo RepoRef) string {
	canonical := canonicalRepoRef(repo)
	return string(repoPlatform(canonical)) + "\x00" + canonical.PlatformHost +
		"\x00" + canonical.Owner + "\x00" + canonical.Name
}

func expandedRepoIdentityKey(repo RepoRef) string {
	if strings.TrimSpace(repo.PlatformExternalID) == "" {
		return ""
	}
	canonical := canonicalRepoRef(repo)
	return string(repoPlatform(canonical)) + "\x00" + canonical.PlatformHost +
		"\x00" + canonical.PlatformExternalID
}

func (s *ExpandedRepoSet) Add(repo RepoRef, providerResolved bool) {
	routeKey := expandedRepoRouteKey(repo)
	identityKey := expandedRepoIdentityKey(repo)
	slot, ok := -1, false
	if identityKey != "" {
		slot, ok = lookupSlot(s.byIdentity, identityKey)
	}
	if !ok {
		slot, ok = lookupSlot(s.byRoute, routeKey)
	}
	if ok {
		// Config-entry provenance is authored only by exact entries; glob
		// refs carry none. Merge it across duplicates in both directions so
		// whichever ref wins the slot, the exact entry stays correlatable
		// on the next reload.
		if providerResolved && !s.resolved[slot] {
			old := s.refs[slot]
			if repo.ConfiguredRepoPath == "" {
				repo.ConfiguredRepoPath = old.ConfiguredRepoPath
			}
			delete(s.byRoute, expandedRepoRouteKey(old))
			if oldIdentity := expandedRepoIdentityKey(old); oldIdentity != "" {
				delete(s.byIdentity, oldIdentity)
			}
			s.refs[slot] = repo
			s.resolved[slot] = true
			s.byRoute[routeKey] = slot
			if identityKey != "" {
				s.byIdentity[identityKey] = slot
			}
		} else if s.refs[slot].ConfiguredRepoPath == "" {
			s.refs[slot].ConfiguredRepoPath = repo.ConfiguredRepoPath
		}
		return
	}
	slot = len(s.refs)
	s.refs = append(s.refs, repo)
	s.resolved = append(s.resolved, providerResolved)
	s.byRoute[routeKey] = slot
	if identityKey != "" {
		s.byIdentity[identityKey] = slot
	}
}

func lookupSlot(index map[string]int, key string) (int, bool) {
	i, ok := index[key]
	if !ok {
		return -1, false
	}
	return i, true
}

func (s *ExpandedRepoSet) Refs() []RepoRef {
	return s.refs
}

func sameConfiguredRepoHost(left, right string) bool {
	if left == "" {
		left = "github.com"
	}
	if right == "" {
		right = "github.com"
	}
	return strings.EqualFold(left, right)
}

// RegisterConfiguredRepoCredentialAliases keeps credential selection on the
// configured route when startup resolution followed a provider rename. The
// syncer registers the same alias when it observes a rename live, but startup
// resolution can adopt the new route before any sync runs — without the
// alias, credential routing has no route for the resolved owner/name and the
// first sync fails on hosts with no owner or fallback route.
func RegisterConfiguredRepoCredentialAliases(
	routers map[string]*HostRouter,
	raw config.Repo,
	expanded []RepoRef,
) {
	if raw.HasNameGlob() {
		return
	}
	if platform.Kind(raw.PlatformOrDefault()) != platform.KindGitHub {
		return
	}
	configured := RouteKey{
		Host:  canonicalRepoHost(raw.PlatformHostOrDefault()),
		Owner: canonicalRepoOwner(strings.TrimSpace(raw.Owner)),
		Name:  canonicalRepoName(strings.TrimSpace(raw.Name)),
	}
	for _, repo := range expanded {
		if strings.EqualFold(repo.Owner, configured.Owner) &&
			strings.EqualFold(repo.Name, configured.Name) {
			continue
		}
		router := routers[repoHost(repo)]
		if router == nil {
			continue
		}
		router.RegisterRepoCredentialAlias(
			repo.Owner, repo.Name, configured, repo.PlatformExternalID,
		)
	}
}
