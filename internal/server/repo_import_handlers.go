package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/config"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

type repoPreviewInput struct {
	Body repoPreviewRequest
}

type repoPreviewRequest struct {
	Provider     string `json:"provider"`
	Host         string `json:"host,omitempty"`
	PlatformHost string `json:"platform_host,omitempty"`
	Owner        string `json:"owner"`
	Pattern      string `json:"pattern"`
}

type repoPreviewOutput = httpapi.BodyOutput[repoPreviewResponse]

type repoPreviewResponse struct {
	Provider     string           `json:"provider"`
	PlatformHost string           `json:"platform_host"`
	Owner        string           `json:"owner"`
	Pattern      string           `json:"pattern"`
	Repos        []repoPreviewRow `json:"repos" nullable:"false"`
}

type repoPreviewRow struct {
	Provider          string  `json:"provider"`
	PlatformHost      string  `json:"platform_host"`
	Owner             string  `json:"owner"`
	Name              string  `json:"name"`
	RepoPath          string  `json:"repo_path"`
	Description       *string `json:"description"`
	Private           bool    `json:"private"`
	Fork              bool    `json:"fork"`
	PushedAt          *string `json:"pushed_at"`
	AlreadyConfigured bool    `json:"already_configured"`
}

type bulkAddReposInput struct {
	Body bulkAddReposRequest
}

type bulkAddReposRequest struct {
	Repos []bulkAddRepoRequest `json:"repos" nullable:"false"`
}

type bulkAddReposOutput = httpapi.CreatedOutput[settingsResponse]

type bulkAddRepoRequest struct {
	Provider     string `json:"provider"`
	Host         string `json:"host,omitempty"`
	PlatformHost string `json:"platform_host,omitempty"`
	Owner        string `json:"owner,omitempty"`
	Name         string `json:"name,omitempty"`
	RepoPath     string `json:"repo_path,omitempty"`
}

type resolvedBulkRepo struct {
	Config config.Repo
	Ref    ghclient.RepoRef
}

func normalizeImportPlatform(provider, host string) (platform.Kind, string, error) {
	if strings.TrimSpace(provider) == "" {
		return "", "", fmt.Errorf("provider is required")
	}
	kind, err := platform.NormalizeKind(provider)
	if err != nil {
		return "", "", err
	}
	normalizedHost, err := config.NormalizePlatformHost(string(kind), host)
	if err != nil {
		return "", "", err
	}
	return kind, normalizedHost, nil
}

func importRequestHost(host, platformHost string) string {
	if strings.TrimSpace(host) != "" {
		return host
	}
	return platformHost
}

func normalizeImportOwnerPattern(
	provider platform.Kind,
	owner, pattern string,
) (string, string, error) {
	owner = strings.TrimSpace(owner)
	pattern = strings.TrimSpace(pattern)
	if owner == "" || pattern == "" {
		return "", "", fmt.Errorf("owner and pattern are required")
	}
	if !platform.AllowsNestedOwner(provider) && strings.Contains(owner, "/") {
		return "", "", fmt.Errorf("owner must not contain /")
	}
	if strings.ContainsAny(owner, "*?[]") {
		return "", "", fmt.Errorf("glob syntax in owner is not supported")
	}
	if strings.Contains(pattern, "/") {
		return "", "", fmt.Errorf("pattern must not contain /")
	}
	if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
		return "", "", fmt.Errorf("invalid glob pattern: %w", err)
	}
	return owner, pattern, nil
}

func normalizeExactRepoInput(raw bulkAddRepoRequest) (config.Repo, error) {
	provider, host, err := normalizeImportPlatform(
		raw.Provider,
		importRequestHost(raw.Host, raw.PlatformHost),
	)
	if err != nil {
		return config.Repo{}, err
	}
	owner := strings.TrimSpace(raw.Owner)
	name := strings.TrimSpace(raw.Name)
	repoPath := strings.Trim(strings.TrimSpace(raw.RepoPath), "/")
	if repoPath != "" {
		if strings.ContainsAny(repoPath, "*?[]") {
			return config.Repo{}, fmt.Errorf("bulk add only accepts exact repositories")
		}
		if owner == "" || name == "" {
			parts := strings.Split(repoPath, "/")
			if len(parts) < 2 || parts[0] == "" || parts[len(parts)-1] == "" {
				return config.Repo{}, fmt.Errorf("repo_path must include owner and name")
			}
			owner = strings.Join(parts[:len(parts)-1], "/")
			name = parts[len(parts)-1]
		}
	}
	if owner == "" || name == "" {
		return config.Repo{}, fmt.Errorf("owner and name are required")
	}
	if !platform.AllowsNestedOwner(provider) && strings.Contains(owner, "/") {
		return config.Repo{}, fmt.Errorf("bulk add only accepts exact owner/name repositories")
	}
	if strings.Contains(name, "/") ||
		strings.ContainsAny(owner, "*?[]") || strings.ContainsAny(name, "*?[]") {
		return config.Repo{}, fmt.Errorf("bulk add only accepts exact owner/name repositories")
	}
	if repoPath == "" {
		repoPath = owner + "/" + name
	}
	repo := config.Repo{
		Owner:        owner,
		Name:         name,
		RepoPath:     repoPath,
		Platform:     string(provider),
		PlatformHost: host,
	}
	if provider == platform.KindGitHub && host == platform.DefaultGitHubHost {
		repo.Platform = ""
		repo.PlatformHost = ""
		repo.RepoPath = ""
	}
	return repo, nil
}

func exactConfiguredRepoSet(repos []config.Repo) map[string]struct{} {
	set := make(map[string]struct{}, len(repos))
	for _, repo := range repos {
		if repo.HasNameGlob() {
			continue
		}
		key := configuredRepoImportKey(repo)
		if key == "" {
			continue
		}
		set[key] = struct{}{}
	}
	return set
}

func configuredRepoImportKey(repo config.Repo) string {
	provider := strings.ToLower(strings.TrimSpace(repo.PlatformOrDefault()))
	host := strings.ToLower(strings.TrimSpace(repo.PlatformHostOrDefault()))
	repoPath := strings.TrimSpace(repo.RepoPath)
	if repoPath == "" {
		repoPath = strings.TrimSpace(repo.Owner) + "/" + strings.TrimSpace(repo.Name)
	}
	if repoPath == "/" {
		return ""
	}
	return provider + "\x00" + host + "\x00" + strings.ToLower(repoPath)
}

func repoRefImportKey(ref ghclient.RepoRef) string {
	provider := strings.ToLower(repoProvider(ref))
	host := strings.ToLower(ref.PlatformHost)
	repoPath := strings.TrimSpace(ref.RepoPath)
	if repoPath == "" {
		repoPath = ref.Owner + "/" + ref.Name
	}
	return provider + "\x00" + host + "\x00" + strings.ToLower(repoPath)
}

func repoImportPatternHasGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[]")
}

func buildRepoPreviewRow(
	repo *gh.Repository,
	fallbackOwner string,
	host string,
	exactConfigured map[string]struct{},
) repoPreviewRow {
	name := repo.GetName()
	canonicalOwner := repo.GetOwner().GetLogin()
	if canonicalOwner == "" {
		canonicalOwner = fallbackOwner
	}
	canonicalOwner = strings.ToLower(canonicalOwner)
	canonicalName := strings.ToLower(name)
	var pushedAt *string
	if repo.PushedAt != nil {
		formatted := repo.PushedAt.Time.UTC().Format(time.RFC3339)
		pushedAt = &formatted
	}
	repoPath := canonicalOwner + "/" + canonicalName
	_, already := exactConfigured[configuredRepoImportKey(config.Repo{
		Owner:        ownerOrFallback(canonicalOwner, fallbackOwner),
		Name:         canonicalName,
		PlatformHost: host,
	})]
	return repoPreviewRow{
		Provider:          "github",
		PlatformHost:      host,
		Owner:             canonicalOwner,
		Name:              canonicalName,
		RepoPath:          repoPath,
		Description:       repo.Description,
		Private:           repo.GetPrivate(),
		Fork:              repo.GetFork(),
		PushedAt:          pushedAt,
		AlreadyConfigured: already,
	}
}

func ownerOrFallback(owner, fallback string) string {
	if owner != "" {
		return owner
	}
	return fallback
}

func buildRepoPreviewRows(
	ctx context.Context,
	client ghclient.Client,
	exactConfigured map[string]struct{},
	owner, pattern string,
	host string,
) ([]repoPreviewRow, error) {
	if !repoImportPatternHasGlob(pattern) {
		repo, err := client.GetRepository(ctx, owner, pattern)
		if err == nil {
			if repo.GetArchived() {
				return []repoPreviewRow{}, nil
			}
			return []repoPreviewRow{
				buildRepoPreviewRow(repo, owner, host, exactConfigured),
			}, nil
		}
	}

	repos, err := client.ListRepositoriesByOwner(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf(
			"list repositories for preview %s/%s: %w", owner, pattern, err,
		)
	}

	rows := make([]repoPreviewRow, 0, len(repos))
	for _, repo := range repos {
		if repo.GetArchived() {
			continue
		}
		name := repo.GetName()
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(name))
		if err != nil {
			return nil, fmt.Errorf("invalid glob pattern: %w", err)
		}
		if !matched {
			continue
		}
		rows = append(rows, buildRepoPreviewRow(repo, owner, host, exactConfigured))
	}
	return rows, nil
}

func buildPlatformRepoPreviewRows(
	ctx context.Context,
	reader platform.RepositoryReader,
	provider platform.Kind,
	host string,
	exactConfigured map[string]struct{},
	owner, pattern string,
) ([]repoPreviewRow, error) {
	if !repoImportPatternHasGlob(pattern) {
		repo, err := reader.GetRepository(ctx, platform.RepoRef{
			Platform: provider,
			Host:     host,
			Owner:    owner,
			Name:     pattern,
			RepoPath: owner + "/" + pattern,
		})
		if err == nil {
			if repo.Archived {
				return []repoPreviewRow{}, nil
			}
			return []repoPreviewRow{
				buildPlatformRepoPreviewRow(
					repo, provider, host, owner, exactConfigured,
				),
			}, nil
		}
	}

	repos, err := reader.ListRepositories(ctx, owner, platform.RepositoryListOptions{})
	if err != nil {
		return nil, fmt.Errorf(
			"list repositories for preview %s/%s: %w", owner, pattern, err,
		)
	}

	rows := make([]repoPreviewRow, 0, len(repos))
	for _, repo := range repos {
		if repo.Archived {
			continue
		}
		name := repo.Ref.Name
		if name == "" {
			name = path.Base(repo.Ref.DisplayName())
		}
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(name))
		if err != nil {
			return nil, fmt.Errorf("invalid glob pattern: %w", err)
		}
		if !matched {
			continue
		}
		rows = append(rows, buildPlatformRepoPreviewRow(
			repo, provider, host, owner, exactConfigured,
		))
	}
	return rows, nil
}

func buildPlatformRepoPreviewRow(
	repo platform.Repository,
	provider platform.Kind,
	host, fallbackOwner string,
	exactConfigured map[string]struct{},
) repoPreviewRow {
	owner := repo.Ref.Owner
	if owner == "" {
		owner = fallbackOwner
	}
	name := repo.Ref.Name
	if name == "" {
		name = path.Base(repo.Ref.DisplayName())
	}
	repoPath := repo.Ref.RepoPath
	if repoPath == "" {
		repoPath = owner + "/" + name
	}
	var pushedAt *string
	if !repo.UpdatedAt.IsZero() {
		formatted := repo.UpdatedAt.UTC().Format(time.RFC3339)
		pushedAt = &formatted
	}
	desc := repo.Description
	var description *string
	if desc != "" {
		description = &desc
	}
	_, already := exactConfigured[configuredRepoImportKey(config.Repo{
		Platform:     string(provider),
		PlatformHost: host,
		Owner:        owner,
		Name:         name,
		RepoPath:     repoPath,
	})]
	return repoPreviewRow{
		Provider:          string(provider),
		PlatformHost:      host,
		Owner:             owner,
		Name:              name,
		RepoPath:          repoPath,
		Description:       description,
		Private:           repo.Private,
		PushedAt:          pushedAt,
		AlreadyConfigured: already,
	}
}

func (s *Server) previewRepos(
	ctx context.Context,
	input *repoPreviewInput,
) (*repoPreviewOutput, error) {
	if s.cfgPath == "" {
		return nil, httpapi.NotFound(httpapi.CodeSettingsUnavailable, "settings not available", nil)
	}

	provider, host, err := normalizeImportPlatform(
		input.Body.Provider,
		importRequestHost(input.Body.Host, input.Body.PlatformHost),
	)
	if err != nil {
		return nil, httpapi.Validation("body.provider", err.Error())
	}
	owner, pattern, err := normalizeImportOwnerPattern(
		provider, input.Body.Owner, input.Body.Pattern,
	)
	if err != nil {
		return nil, httpapi.Validation("body", err.Error())
	}

	s.cfgMu.Lock()
	repos := slices.Clone(s.cfg.Repos)
	s.cfgMu.Unlock()

	// Provider read failures route through the shared mapping so a
	// missing token during token-file rotation surfaces as 400
	// badRequest like the sync and runtime paths, not a 502.
	var rows []repoPreviewRow
	if provider == platform.KindGitHub {
		client, err := s.syncer.DirectClientForHost(host)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, "github", host)
		}
		rows, err = buildRepoPreviewRows(
			ctx, client, exactConfiguredRepoSet(repos), owner, pattern, host,
		)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, "github", host)
		}
	} else {
		reader, err := s.syncer.RepositoryReader(provider, host)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, string(provider), host)
		}
		rows, err = buildPlatformRepoPreviewRows(
			ctx, reader, provider, host, exactConfiguredRepoSet(repos), owner, pattern,
		)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, string(provider), host)
		}
	}
	return &repoPreviewOutput{
		Body: repoPreviewResponse{
			Provider:     string(provider),
			PlatformHost: host,
			Owner:        owner,
			Pattern:      pattern,
			Repos:        rows,
		},
	}, nil
}

func validateBulkExactRepos(
	ctx context.Context,
	syncer *ghclient.Syncer,
	candidates []config.Repo,
) ([]resolvedBulkRepo, error) {
	seenInput := make(map[string]struct{}, len(candidates))
	seenResolved := make(map[string]struct{}, len(candidates))
	resolved := make([]resolvedBulkRepo, 0, len(candidates))
	for _, candidate := range candidates {
		key := configuredRepoImportKey(candidate)
		if _, ok := seenInput[key]; ok {
			continue
		}
		seenInput[key] = struct{}{}

		_, refs, err := syncer.ResolveConfiguredRepo(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if len(refs) != 1 {
			return nil, fmt.Errorf(
				"resolve exact repo %s/%s returned %d matches",
				candidate.Owner, candidate.Name, len(refs),
			)
		}
		ref := refs[0]
		resolvedKey := repoRefImportKey(ref)
		if _, ok := seenResolved[resolvedKey]; ok {
			continue
		}
		seenResolved[resolvedKey] = struct{}{}
		resolved = append(resolved, resolvedBulkRepo{
			Config: configFromResolvedRepo(candidate, ref),
			Ref:    ref,
		})
	}
	return resolved, nil
}

func configFromResolvedRepo(candidate config.Repo, ref ghclient.RepoRef) config.Repo {
	repo := config.Repo{
		Owner:        ref.Owner,
		Name:         ref.Name,
		RepoPath:     ref.RepoPath,
		Platform:     string(ref.Platform),
		PlatformHost: ref.PlatformHost,
		TokenEnv:     candidate.TokenEnv,
	}
	if repo.Platform == "" || repo.Platform == "github" {
		repo.Platform = ""
		if repo.PlatformHost == "github.com" {
			repo.PlatformHost = ""
		}
		repo.RepoPath = ""
	}
	return repo
}

// bulkApplyError is a sentinel carrying the wire problem produced by
// applyBulkExactRepos so the handler can return it directly without
// re-classifying status codes.
type bulkApplyError struct {
	problem huma.StatusError
}

func (e *bulkApplyError) Error() string { return e.problem.Error() }

func (s *Server) applyBulkExactRepos(
	ctx context.Context,
	resolved []resolvedBulkRepo,
) (settingsResponse, error) {
	s.cfgMu.Lock()
	existing := exactConfiguredRepoSet(s.cfg.Repos)
	addConfigs := make([]config.Repo, 0, len(resolved))
	addRefs := make([]ghclient.RepoRef, 0, len(resolved))
	for _, repo := range resolved {
		key := configuredRepoImportKey(repo.Config)
		if _, ok := existing[key]; ok {
			continue
		}
		existing[key] = struct{}{}
		addConfigs = append(addConfigs, repo.Config)
		addRefs = append(addRefs, repo.Ref)
	}
	if len(addConfigs) == 0 {
		s.cfgMu.Unlock()
		return settingsResponse{}, &bulkApplyError{problem: httpapi.BadRequest(
			httpapi.CodeBadRequest,
			"all selected repositories are already configured",
			nil,
		)}
	}

	prev := slices.Clone(s.cfg.Repos)
	s.cfg.Repos = append(s.cfg.Repos, addConfigs...)
	if err := s.cfg.Validate(); err != nil {
		s.cfg.Repos = prev
		s.cfgMu.Unlock()
		return settingsResponse{}, &bulkApplyError{problem: httpapi.BadRequest(
			httpapi.CodeBadRequest, err.Error(), nil,
		)}
	}
	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.cfg.Repos = prev
		s.cfgMu.Unlock()
		return settingsResponse{}, &bulkApplyError{problem: httpapi.Internal(
			"save config: " + err.Error(),
		)}
	}
	if err := s.persistResolvedRepos(ctx, addRefs); err != nil {
		s.cfg.Repos = prev
		s.cfgMu.Unlock()
		return settingsResponse{}, &bulkApplyError{problem: httpapi.Internal(err.Error())}
	}
	s.mergeTrackedRepos(addRefs)
	s.applyWorkspaceConfigLocked()
	s.cfgMu.Unlock()

	body, err := s.buildLocalSettingsResponse(ctx)
	if err != nil {
		return settingsResponse{}, &bulkApplyError{
			problem: httpapi.Internal(err.Error()),
		}
	}
	return body, nil
}

func (s *Server) bulkAddRepos(
	ctx context.Context,
	input *bulkAddReposInput,
) (*bulkAddReposOutput, error) {
	if s.cfgPath == "" {
		return nil, httpapi.NotFound(httpapi.CodeSettingsUnavailable, "settings not available", nil)
	}

	if len(input.Body.Repos) == 0 {
		return nil, httpapi.Validation("body.repos", "repos are required")
	}

	candidates := make([]config.Repo, 0, len(input.Body.Repos))
	s.cfgMu.Lock()
	existing := exactConfiguredRepoSet(s.cfg.Repos)
	s.cfgMu.Unlock()
	for _, raw := range input.Body.Repos {
		repo, err := normalizeExactRepoInput(raw)
		if err != nil {
			return nil, httpapi.Validation("body.repos", err.Error())
		}
		key := configuredRepoImportKey(repo)
		if _, ok := existing[key]; ok {
			continue
		}
		candidates = append(candidates, repo)
	}
	if len(candidates) == 0 {
		return nil, httpapi.BadRequest(
			httpapi.CodeBadRequest,
			"all selected repositories are already configured",
			nil,
		)
	}

	resolved, err := validateBulkExactRepos(ctx, s.syncer, candidates)
	if err != nil {
		return nil, classifyResolveProblem(err)
	}
	resp, err := s.applyBulkExactRepos(ctx, resolved)
	if err != nil {
		if bae, ok := errors.AsType[*bulkApplyError](err); ok {
			return nil, bae.problem
		}
		return nil, httpapi.Internal(err.Error())
	}

	s.syncer.TriggerRun(context.WithoutCancel(ctx))
	return &bulkAddReposOutput{Status: http.StatusCreated, Body: resp}, nil
}
