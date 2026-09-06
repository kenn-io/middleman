package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	forgejoclient "go.kenn.io/forge/platform/forgejo"
	giteaclient "go.kenn.io/forge/platform/gitea"
	gitlabclient "go.kenn.io/forge/platform/gitlab"
)

type providerFactory func(context.Context, providerFactoryInput) (providerFactoryOutput, error)

type providerFactoryInput struct {
	host          string
	baseURL       string
	allowInsecure bool
	tokenSource   tokenauth.Source
	rateTracker   *github.RateTracker
	budget        *github.SyncBudget
	quotaRegistry *github.QuotaRegistry
}

type providerFactoryOutput struct {
	githubClient github.Client
	provider     platform.Provider
}

func (input providerFactoryInput) rateObserver() platform.RateObserver {
	if input.rateTracker == nil {
		return nil
	}
	return input.rateTracker
}

type graphQLRateTrackerSetter interface {
	SetGraphQLRateTracker(platform.RateObserver)
}

type writeRateTrackerSetter interface {
	SetWriteRateTracker(platform.RateObserver)
	SetWriteGraphQLRateTracker(platform.RateObserver)
}

type githubIdentityRuntime struct {
	identity github.GitHubIdentity
	budget   *github.SyncBudget
	rest     *github.RateTracker
	graphql  *github.RateTracker
}

type mutationTokenSource struct {
	tokenauth.Source
}

func (s mutationTokenSource) Token(ctx context.Context) (string, error) {
	return s.Source.Token(tokenauth.WithMutationAuth(ctx))
}

type optionalMutationTokenSource struct {
	tokenauth.Source
}

func (s optionalMutationTokenSource) Token(ctx context.Context) (string, error) {
	token, err := s.Source.Token(tokenauth.WithMutationAuth(ctx))
	if errors.Is(err, tokenauth.ErrMissingToken) {
		return "", nil
	}
	return token, err
}

type identityBoundMutationTokenSource struct {
	tokenauth.Source
	writeIdentity github.IdentityKey
}

func (s identityBoundMutationTokenSource) Token(ctx context.Context) (string, error) {
	if s.writeIdentity.Principal == "" {
		return "", github.ErrMissingWriteIdentity
	}
	return s.Source.Token(tokenauth.WithMutationAuth(ctx))
}

// missingRouteTokenSource fails closed for a repository that no configured
// credential route serves. Managed Git treats a nil source as permission to run
// without credentials, so returning nil would let an unrouted repository reach
// the remote unauthenticated and quietly succeed whenever it happens to be
// public, instead of reporting that no credential route covers it.
type missingRouteTokenSource struct {
	host  string
	owner string
	name  string
}

func (s missingRouteTokenSource) Token(context.Context) (string, error) {
	return "", &github.MissingRouteError{
		Host: s.host, Owner: s.owner, Name: s.name,
	}
}

func (missingRouteTokenSource) Invalidate(string) {}

func (missingRouteTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{}
}

type githubCredentialRoute struct {
	key                 tokenauth.Key
	source              tokenauth.Source
	client              github.Client
	fetcher             *github.GraphQLFetcher
	discoveryOwner      string
	readIdentity        github.IdentityKey
	writeIdentity       github.IdentityKey
	archiveKey          tokenauth.Key
	archiveSource       tokenauth.Source
	archiveReadIdentity github.IdentityKey
}

type gitCredentialRoute struct {
	source        tokenauth.Source
	writeIdentity github.IdentityKey
	identityBound bool
	required      bool
}

// gitStartup owns only spoke-local Git authentication. Building it registers
// lazy credential sources but never resolves an identity or calls a provider
// API, so spokes can retain clone/fetch/push without constructing the
// provider control plane.
type gitStartup struct {
	cloneSources map[tokenauth.Key]tokenauth.Source
	cloneAuth    map[string]tokenauth.Source
	githubRoutes map[tokenauth.Key]gitCredentialRoute
	githubHosts  map[string]struct{}
}

func buildGitStartup(cfg *config.Config, set *tokenauth.SourceSet) (gitStartup, error) {
	if cfg == nil || set == nil {
		return gitStartup{}, errors.New("git startup requires config and token sources")
	}
	if err := cfg.ValidateRepoTokenSourceConsistency(); err != nil {
		return gitStartup{}, err
	}
	startup := gitStartup{
		cloneSources: make(map[tokenauth.Key]tokenauth.Source),
		cloneAuth:    make(map[string]tokenauth.Source),
		githubRoutes: make(map[tokenauth.Key]gitCredentialRoute),
		githubHosts:  make(map[string]struct{}),
	}
	for _, plan := range cfg.ProviderTokenSources() {
		desc := plan.Descriptor
		source := set.Upsert(desc)
		hostKey := tokenauth.Key{Platform: desc.Key.Platform, Host: desc.Key.Host}
		if startup.cloneSources[hostKey] == nil {
			startup.cloneSources[hostKey] = source
		}
		if desc.Key.Platform == string(platform.KindGitHub) {
			startup.githubHosts[desc.Key.Host] = struct{}{}
			startup.githubRoutes[desc.Key] = gitCredentialRoute{
				source: source, required: plan.Required,
			}
		}
	}
	for _, desc := range cfg.CloneTokenDescriptors() {
		startup.cloneAuth[desc.Key.Host] = set.Upsert(desc)
	}
	for _, source := range startup.cloneSources {
		if source == nil {
			continue
		}
		desc := source.Descriptor()
		host := desc.Key.Host
		if startup.cloneAuth[host] != nil ||
			(desc.Key.Platform == string(platform.KindGitHub) && desc.Key.Scope != "") {
			continue
		}
		desc.Key = tokenauth.CloneKey(host)
		startup.cloneAuth[host] = set.Upsert(desc)
	}
	return startup, nil
}

// ApplyProviderControlPlane upgrades healthy GitHub routes with their verified
// mutation identity. Git remains usable when a provider host is degraded; in
// that case its lazy config-derived route remains in place.
func (s *gitStartup) ApplyProviderControlPlane(control *providerControlPlane) {
	if s == nil || control == nil {
		return
	}
	for key, route := range control.githubRoutes {
		s.githubRoutes[key] = gitCredentialRoute{
			source: route.source, writeIdentity: route.writeIdentity,
			identityBound: true, required: true,
		}
	}
}

func (s *gitStartup) SourceForRepo(
	platformName, host, owner, name string,
) tokenauth.Source {
	if s == nil {
		return nil
	}
	platformName = strings.ToLower(strings.TrimSpace(platformName))
	host = strings.ToLower(strings.TrimSpace(host))
	providerSource := s.cloneSources[tokenauth.Key{Platform: platformName, Host: host}]
	if platformName != string(platform.KindGitHub) {
		return providerSource
	}
	owner = strings.ToLower(strings.TrimSpace(owner))
	name = strings.ToLower(strings.TrimSpace(name))
	for _, scope := range []string{"repo:" + owner + "/" + name, "owner:" + owner, ""} {
		route, ok := s.githubRoutes[tokenauth.Key{
			Platform: string(platform.KindGitHub), Host: host, Scope: scope,
		}]
		if !ok || route.source == nil {
			continue
		}
		if route.identityBound {
			return identityBoundMutationTokenSource{
				Source: route.source, writeIdentity: route.writeIdentity,
			}
		}
		if route.required {
			return mutationTokenSource{Source: route.source}
		}
		return optionalMutationTokenSource{Source: route.source}
	}
	if _, routed := s.githubHosts[host]; routed {
		return missingRouteTokenSource{host: host, owner: owner, name: name}
	}
	return providerSource
}

func (s *gitStartup) FallbackSource(host string) tokenauth.Source {
	if s == nil {
		return nil
	}
	host = strings.ToLower(strings.TrimSpace(host))
	clone := s.cloneAuth[host]
	if clone != nil && len(clone.Descriptor().Candidates) == 0 {
		return clone
	}
	if route, ok := s.githubRoutes[tokenauth.Key{
		Platform: string(platform.KindGitHub), Host: host,
	}]; ok && route.source != nil {
		if route.identityBound {
			return identityBoundMutationTokenSource{
				Source: route.source, writeIdentity: route.writeIdentity,
			}
		}
		if route.required {
			return mutationTokenSource{Source: route.source}
		}
		return optionalMutationTokenSource{Source: route.source}
	}
	return clone
}

type providerControlPlane struct {
	registry             *platform.Registry
	rateTrackers         map[string]*github.RateTracker
	writeRateTrackers    map[string]*github.RateTracker
	writeGQLRateTrackers map[string]*github.RateTracker
	budgets              map[string]*github.SyncBudget
	fetchers             map[string]*github.GraphQLFetcher
	githubRoutes         map[tokenauth.Key]githubCredentialRoute
	githubIdentities     map[string]*githubIdentityRuntime
	githubRouters        map[string]*github.HostRouter
	githubClients        map[string]github.Client
	ratePrincipalLabels  map[string]string
	quotaRegistry        *github.QuotaRegistry
}

func defaultProviderFactories() map[string]providerFactory {
	return map[string]providerFactory{
		string(platform.KindGitHub): func(ctx context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			// No credential router on this path, so reads and mutations both
			// account to the host-wide chain.
			hostIdentity := github.HostIdentity(input.host)
			client, err := github.NewClient(
				input.tokenSource, input.host, input.rateTracker, input.budget,
				github.WithQuotaAccounting(
					input.quotaRegistry, hostIdentity, hostIdentity,
				),
			)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			return providerFactoryOutput{
				githubClient: client,
			}, nil
		},
		string(platform.KindGitLab): func(ctx context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			client, err := gitlabclient.NewClient(
				input.host, input.tokenSource,
				gitlabclient.WithRateTracker(input.rateObserver()),
				gitlabclient.WithOptionalRequestContext(github.WithoutEssentialSyncBudget),
				gitlabclient.WithTransport(github.WrapSyncBudgetTransport(http.DefaultTransport, input.budget)),
			)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			return providerFactoryOutput{provider: client}, nil
		},
		string(platform.KindForgejo): func(ctx context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			options := []forgejoclient.ClientOption{
				forgejoclient.WithRateTracker(input.rateObserver()),
				forgejoclient.WithTransport(github.WrapSyncBudgetTransport(http.DefaultTransport, input.budget)),
			}
			client, err := forgejoclient.NewClient(input.host, input.tokenSource, options...)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			version, err := client.ServerVersion(ctx)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			options = append(options, forgejoclient.WithServerVersion(version))
			client, err = forgejoclient.NewClient(input.host, input.tokenSource, options...)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			return providerFactoryOutput{provider: client}, nil
		},
		string(platform.KindGitea): func(ctx context.Context, input providerFactoryInput) (providerFactoryOutput, error) {
			options := []giteaclient.ClientOption{
				giteaclient.WithBaseURL(input.baseURL, input.allowInsecure),
				giteaclient.WithRateTracker(input.rateObserver()),
				giteaclient.WithTransport(github.WrapSyncBudgetTransport(http.DefaultTransport, input.budget)),
			}
			client, err := giteaclient.NewClient(input.host, input.tokenSource, options...)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			version, err := client.ServerVersion(ctx)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			options = append(options, giteaclient.WithServerVersion(version))
			client, err = giteaclient.NewClient(input.host, input.tokenSource, options...)
			if err != nil {
				return providerFactoryOutput{}, err
			}
			return providerFactoryOutput{provider: client}, nil
		},
	}
}

func collectProviderTokenSources(
	ctx context.Context,
	cfg *config.Config,
	set *tokenauth.SourceSet,
) (map[string]tokenauth.Source, error) {
	return providerTokenSources(ctx, cfg, set, true, false)
}

// collectProviderTokenSourcesDegraded resolves provider tokens like
// collectProviderTokenSources, but a host whose credentials fail is excluded
// with a warning instead of failing startup, so one provider outage cannot
// stop sync for healthy hosts. Config-consistency errors still fail.
func collectProviderTokenSourcesDegraded(
	ctx context.Context,
	cfg *config.Config,
	set *tokenauth.SourceSet,
) (map[string]tokenauth.Source, error) {
	return providerTokenSources(ctx, cfg, set, true, true)
}

func registerProviderTokenSources(
	cfg *config.Config,
	set *tokenauth.SourceSet,
) (map[string]tokenauth.Source, error) {
	return providerTokenSources(context.Background(), cfg, set, false, false)
}

func providerTokenSources(
	ctx context.Context,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	resolve bool,
	degradeFailedHosts bool,
) (map[string]tokenauth.Source, error) {
	if err := cfg.ValidateRepoTokenSourceConsistency(); err != nil {
		return nil, err
	}
	providerSources := make(map[string]tokenauth.Source, len(cfg.Repos)+len(cfg.Platforms)+1)
	failedHosts := make(map[string]struct{})
	for _, plan := range cfg.ProviderTokenSources() {
		desc := plan.Descriptor
		key := providerHostKey(desc.Key.Platform, desc.Key.Host)
		if _, failed := failedHosts[key]; failed {
			continue
		}
		_, seen := providerSources[key]
		tokenCtx := ctx
		if plan.GitHubOwner != "" {
			tokenCtx = tokenauth.WithGitHubOwner(tokenCtx, plan.GitHubOwner)
		}
		if plan.ArchiveDescriptor.Key.Host != "" {
			archiveDesc := plan.ArchiveDescriptor
			archiveSrc := set.Upsert(archiveDesc)
			if resolve {
				if _, err := archiveSrc.Token(tokenCtx); err != nil {
					if degradeFailedHosts {
						// Archive capacity is independent of ordinary sync.
						// Keep processing the ordinary descriptor so a broken
						// archive App cannot take the whole host offline.
						slog.Warn(
							"provider archive credentials unavailable; serving cached data for it without sync",
							"platform", archiveDesc.Key.Platform,
							"host", archiveDesc.Key.Host,
							"err", err,
						)
					} else {
						label := fmt.Sprintf(
							"%s host %s archive", archiveDesc.Key.Platform,
							archiveDesc.Key.Host,
						)
						if plan.GitHubOwner != "" {
							label = fmt.Sprintf("%s owner %s", label, plan.GitHubOwner)
						}
						return nil, fmt.Errorf(
							"no token for %s via %s: %w",
							label, archiveDesc.SafeString(), err,
						)
					}
				}
			}
		}
		src := set.Upsert(desc)
		if resolve {
			if _, err := src.Token(tokenCtx); err != nil {
				if plan.ArchiveOnly && errors.Is(err, tokenauth.ErrMissingToken) {
					// An archive-only App is intentionally independent of the
					// ordinary PAT chain. Keep the host provider present so its
					// routed client can serve archive requests, while the route
					// builder omits the unusable ordinary client.
					if !seen {
						providerSources[key] = src
					}
					continue
				}
				if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
					continue
				}
				if degradeFailedHosts {
					// Any credential failure excludes the whole host: syncing
					// with a partial credential chain could select the wrong
					// route for repositories whose token did not resolve.
					failedHosts[key] = struct{}{}
					delete(providerSources, key)
					slog.Warn(
						"provider host credentials unavailable; serving cached data for it without sync",
						"platform", desc.Key.Platform,
						"host", desc.Key.Host,
						"err", err,
					)
					continue
				}
				label := fmt.Sprintf("%s host %s", desc.Key.Platform, desc.Key.Host)
				if plan.GitHubOwner != "" {
					label = fmt.Sprintf("%s owner %s", label, plan.GitHubOwner)
				}
				if plan.Required {
					return nil, fmt.Errorf("no token for %s via %s: %w", label, desc.SafeString(), err)
				}
				return nil, fmt.Errorf(
					"read optional token for %s via %s: %w",
					label, desc.SafeString(), err,
				)
			}
		}
		if !seen {
			providerSources[key] = src
		}
	}
	return providerSources, nil
}

type serveControlPlanes struct {
	Git      gitStartup
	Provider *providerControlPlane
}

// buildServeControlPlanes is the role boundary for external provider work.
// Nodes return after constructing lazy Git routing; only hubs resolve
// provider credentials and construct API clients, rate trackers, and budgets.
func buildServeControlPlanes(
	ctx context.Context,
	database *db.DB,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	factories map[string]providerFactory,
	resolver github.IdentityResolver,
	disableSync bool,
) (serveControlPlanes, error) {
	gitRoutes, err := buildGitStartup(cfg, set)
	if err != nil {
		return serveControlPlanes{}, err
	}
	result := serveControlPlanes{Git: gitRoutes}
	if cfg.Fleet.RoleOrDefault() == config.FleetRoleSpoke {
		return result, nil
	}
	var providerSources map[string]tokenauth.Source
	if disableSync {
		providerSources, err = registerProviderTokenSources(cfg, set)
	} else {
		providerSources, err = collectProviderTokenSourcesDegraded(ctx, cfg, set)
		if err != nil {
			slog.Warn(
				"provider credentials unavailable; serving cached data without provider sync",
				"err", err,
			)
			providerSources = nil
		}
	}
	if err != nil && disableSync {
		return serveControlPlanes{}, err
	}
	if disableSync || providerSources == nil {
		resolver = nil
	}
	control, err := buildProviderControlPlaneForServe(
		ctx, database, cfg, set, providerSources, factories, resolver, disableSync,
	)
	if err != nil {
		return serveControlPlanes{}, err
	}
	result.Provider = &control
	result.Git.ApplyProviderControlPlane(result.Provider)
	return result, nil
}

// providerHostStartupError marks a startup failure attributable to one
// provider host so degraded startup can drop that host and keep the rest.
type providerHostStartupError struct {
	platformName string
	host         string
	err          error
}

func (e *providerHostStartupError) Error() string { return e.err.Error() }

func (e *providerHostStartupError) Unwrap() error { return e.err }

func buildProviderControlPlaneForServe(
	ctx context.Context,
	database *db.DB,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	providerSources map[string]tokenauth.Source,
	factories map[string]providerFactory,
	resolver github.IdentityResolver,
	disableSync bool,
) (providerControlPlane, error) {
	if disableSync {
		return buildProviderControlPlane(
			ctx, database, cfg, set, providerSources, factories, resolver,
		)
	}
	return buildProviderControlPlaneOrDegraded(
		ctx, database, cfg, set, providerSources, factories, resolver,
	)
}

// buildProviderControlPlaneOrDegraded keeps the local UI available when a remote
// provider cannot be initialized. Failures attributable to one provider host
// drop only that host, so healthy hosts keep syncing; anything else falls
// back to an empty provider registry. The database already contains the last
// successful sync, and dropped hosts serve cached data until the daemon is
// restarted after the provider recovers.
func buildProviderControlPlaneOrDegraded(
	ctx context.Context,
	database *db.DB,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	providerSources map[string]tokenauth.Source,
	factories map[string]providerFactory,
	resolver github.IdentityResolver,
) (providerControlPlane, error) {
	sources := providerSources
	for {
		startup, err := buildProviderControlPlane(
			ctx, database, cfg, set, sources, factories, resolver,
		)
		if err == nil {
			return startup, nil
		}
		if hostErr, ok := errors.AsType[*providerHostStartupError](err); ok {
			key := providerHostKey(hostErr.platformName, hostErr.host)
			if _, ok := sources[key]; ok {
				slog.Warn(
					"provider host startup unavailable; serving cached data for it without sync",
					"platform", hostErr.platformName,
					"host", hostErr.host,
					"err", hostErr.err,
				)
				next := make(map[string]tokenauth.Source, len(sources)-1)
				for k, v := range sources {
					if k != key {
						next[k] = v
					}
				}
				sources = next
				continue
			}
		}
		slog.Warn(
			"provider startup unavailable; serving cached data without provider sync",
			"err", err,
		)
		return buildProviderControlPlane(ctx, database, cfg, set, nil, factories, nil)
	}
}

func buildProviderControlPlane(
	ctx context.Context,
	database *db.DB,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	providerSources map[string]tokenauth.Source,
	factories map[string]providerFactory,
	resolver github.IdentityResolver,
) (providerControlPlane, error) {
	if err := cfg.ValidateRepoTokenSourceConsistency(); err != nil {
		return providerControlPlane{}, err
	}
	budgetPerHour := cfg.BudgetPerHour()
	startup := providerControlPlane{
		rateTrackers:         make(map[string]*github.RateTracker, len(providerSources)),
		writeRateTrackers:    make(map[string]*github.RateTracker, len(providerSources)),
		writeGQLRateTrackers: make(map[string]*github.RateTracker, len(providerSources)),
		budgets:              make(map[string]*github.SyncBudget, len(providerSources)),
		fetchers:             make(map[string]*github.GraphQLFetcher, len(providerSources)),
		githubRoutes:         make(map[tokenauth.Key]githubCredentialRoute),
		githubIdentities:     make(map[string]*githubIdentityRuntime),
		githubRouters:        make(map[string]*github.HostRouter),
		githubClients:        make(map[string]github.Client),
		ratePrincipalLabels:  make(map[string]string),
		quotaRegistry:        github.NewQuotaRegistry(),
	}
	if resolver != nil {
		if err := buildGitHubIdentityRuntimes(
			ctx, database, cfg, set, resolver, budgetPerHour,
			providerSources, &startup,
		); err != nil {
			return providerControlPlane{}, err
		}
		if err := buildGitHubRouteClients(&startup); err != nil {
			return providerControlPlane{}, err
		}
	}
	clients := make(map[string]github.Client, len(providerSources))
	providers := make([]platform.Provider, 0, len(providerSources))
	githubHosts := make(map[string]struct{}, len(providerSources))
	for key, tokenSource := range providerSources {
		platformName, host := splitProviderHostKey(key)
		rateKey := github.RateBucketKey(platformName, host, "host")
		routedGitHub := platformName == string(platform.KindGitHub) && startup.githubClients[host] != nil
		if !routedGitHub {
			if _, ok := startup.rateTrackers[rateKey]; !ok {
				startup.rateTrackers[rateKey] = github.NewPlatformRateTracker(
					database, platformName, host, "host", "rest",
				)
			}
			if budgetPerHour > 0 {
				if _, ok := startup.budgets[rateKey]; !ok {
					startup.budgets[rateKey] = github.NewSyncBudgetWithEssentialReserve(budgetPerHour)
				}
			}
		}
		factory, ok := factories[platformName]
		if !ok {
			return providerControlPlane{}, fmt.Errorf("unsupported platform %q", platformName)
		}
		transportConfig := providerTransportConfig(cfg, platformName, host)
		built, err := factory(ctx, providerFactoryInput{
			host:          host,
			baseURL:       transportConfig.BaseURL,
			allowInsecure: transportConfig.AllowInsecure,
			tokenSource:   tokenSource,
			rateTracker:   startup.rateTrackers[rateKey],
			budget:        startup.budgets[rateKey],
			quotaRegistry: startup.quotaRegistry,
		})
		if err != nil {
			return providerControlPlane{}, &providerHostStartupError{
				platformName: platformName,
				host:         host,
				err: fmt.Errorf(
					"create %s client for %s: %w", platformLabel(platformName), host, err,
				),
			}
		}
		if built.githubClient != nil {
			if routed := startup.githubClients[host]; routed != nil {
				clients[host] = routed
			} else {
				clients[host] = built.githubClient
			}
			githubHosts[host] = struct{}{}
		}
		if built.provider != nil {
			providers = append(providers, built.provider)
		}
	}
	registry, err := github.NewProviderRegistry(clients, providers...)
	if err != nil {
		return providerControlPlane{}, fmt.Errorf("create provider registry: %w", err)
	}
	startup.registry = registry
	for host := range githubHosts {
		if startup.githubRouters[host] != nil {
			continue
		}
		rateKey := github.RateBucketKey(string(platform.KindGitHub), host, "host")
		gqlRT := github.NewPlatformRateTracker(database, string(platform.KindGitHub), host, "host", "graphql")
		if setter, ok := clients[host].(graphQLRateTrackerSetter); ok {
			setter.SetGraphQLRateTracker(gqlRT)
		}
		// Hosts whose sync reads ride a GitHub App split off the user's
		// PAT for writes; only those get dedicated write trackers, so
		// write availability gates on the budget writes actually
		// consume. The split is read from the host's effective
		// credential chain, not from [[github_apps]] config alone: a
		// host whose repos all carry terminal token overrides never
		// uses the app candidate, and an empty write tracker would
		// shadow the shared trackers exhausted sync had observed.
		hostSource := providerSources[providerHostKey(
			string(platform.KindGitHub), host,
		)]
		if hostSource != nil && hostSource.Descriptor().HasActiveGitHubApp() {
			if setter, ok := clients[host].(writeRateTrackerSetter); ok {
				writeRT := github.NewPlatformRateTracker(
					database, string(platform.KindGitHub), host, "host", "rest_write",
				)
				writeGQLRT := github.NewPlatformRateTracker(
					database, string(platform.KindGitHub), host, "host", "graphql_write",
				)
				setter.SetWriteRateTracker(writeRT)
				setter.SetWriteGraphQLRateTracker(writeGQLRT)
				startup.writeRateTrackers[rateKey] = writeRT
				startup.writeGQLRateTrackers[rateKey] = writeGQLRT
			}
		}
		source := providerSources[providerHostKey(
			string(platform.KindGitHub), host,
		)]
		startup.fetchers[host] = github.NewGraphQLFetcher(
			source, host, gqlRT, startup.budgets[rateKey],
			github.WithGraphQLQuotaAccounting(
				startup.quotaRegistry, github.HostIdentity(host),
			),
		)
	}
	return startup, nil
}

func providerTransportConfig(
	cfg *config.Config, platformName, host string,
) config.PlatformConfig {
	for _, configured := range cfg.Platforms {
		if configured.Type == platformName && configured.Host == host {
			return configured
		}
	}
	return config.PlatformConfig{}
}

func buildGitHubIdentityRuntimes(
	ctx context.Context,
	database *db.DB,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	resolver github.IdentityResolver,
	budgetPerHour int,
	providerSources map[string]tokenauth.Source,
	startup *providerControlPlane,
) error {
	if cfg == nil || set == nil || resolver == nil {
		return nil
	}
	// Hosts absent from providerSources were excluded (credentials failed or
	// nothing resolved); their identity runtimes and routes stay absent too.
	allPlans := githubCredentialPlans(cfg)
	plans := allPlans[:0:0]
	for _, plan := range allPlans {
		key := providerHostKey(
			plan.Descriptor.Key.Platform, plan.Descriptor.Key.Host,
		)
		if _, ok := providerSources[key]; ok {
			plans = append(plans, plan)
		}
	}
	requiredHosts := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.Required {
			requiredHosts[plan.Descriptor.Key.Host] = struct{}{}
		}
	}
	resolvedPATs := make(map[string]resolvedGitHubPAT, len(plans))
	for _, plan := range plans {
		desc := plan.Descriptor
		// Scoped routes already carry every credential needed for their
		// repositories, so an implicit default env/gh CLI fallback is probed
		// best-effort: a valid token keeps ownerless APIs served, while a
		// missing or invalid one is skipped with a warning instead of
		// failing startup. Explicit host fallbacks still fail hard.
		bestEffort := !plan.Required && desc.Key.Scope == "" &&
			len(requiredHosts) > 0 && !hasExplicitGitHubFallback(cfg, desc.Key.Host)
		var source tokenauth.Source
		app, hasApp := activeGitHubAppCandidate(desc, plan.GitHubOwner)
		var writeIdentity, readIdentity github.GitHubIdentity
		ordinaryUnavailable := false
		if plan.ArchiveOnly {
			// Archive-only routes are allowed to have no ordinary PAT. Keep
			// building the route so the independent archive client remains
			// available; ordinary operations fail closed through the router.
			source = set.Upsert(desc)
			resolvedWrite, err := resolveGitHubPATIdentity(
				ctx, resolver, desc.Key.Host, source, resolvedPATs,
			)
			if err != nil && errors.Is(err, tokenauth.ErrMissingToken) {
				ordinaryUnavailable = true
			} else if err != nil {
				return &providerHostStartupError{
					platformName: desc.Key.Platform,
					host:         desc.Key.Host,
					err: fmt.Errorf(
						"resolve GitHub identity for %s via %s: %w",
						desc.Key.Scope, desc.SafeString(), err,
					),
				}
			} else {
				writeIdentity = resolvedWrite.identity
				if writeIdentity.Key.Principal != "" {
					source = github.BindSourceIdentity(
						source, desc.Key.Host, writeIdentity.Key,
						resolvedWrite.token, resolver,
					)
				}
			}
		}
		if !ordinaryUnavailable {
			if !plan.ArchiveOnly {
				source = set.Upsert(desc)
				resolvedWrite, err := resolveGitHubPATIdentity(
					ctx, resolver, desc.Key.Host, source, resolvedPATs,
				)
				writeIdentity = resolvedWrite.identity
				if err != nil {
					if errors.Is(err, tokenauth.ErrMissingToken) && hasApp {
						writeIdentity = github.GitHubIdentity{}
					} else if !plan.Required && errors.Is(err, tokenauth.ErrMissingToken) {
						continue
					} else if bestEffort {
						slog.Warn(
							"skipping implicit GitHub host fallback; ownerless APIs stay unrouted until it resolves",
							"host", desc.Key.Host,
							"source", desc.SafeString(),
							"error", err,
						)
						continue
					} else {
						return &providerHostStartupError{
							platformName: desc.Key.Platform,
							host:         desc.Key.Host,
							err: fmt.Errorf(
								"resolve GitHub identity for %s via %s: %w",
								desc.Key.Scope, desc.SafeString(), err,
							),
						}
					}
				}
				if writeIdentity.Key.Principal != "" {
					source = github.BindSourceIdentity(
						source, desc.Key.Host, writeIdentity.Key,
						resolvedWrite.token, resolver,
					)
				}
			}
			if hasApp {
				if app.InstallationID <= 0 {
					return fmt.Errorf(
						"resolve GitHub identity for %s via %s: invalid installation id %d",
						desc.Key.Scope, desc.SafeString(), app.InstallationID,
					)
				}
				readIdentity = github.InstallationIdentity(
					desc.Key.Host, app.InstallationID,
				)
			} else {
				readIdentity = writeIdentity
			}
			ensureGitHubIdentityRuntime(
				database, budgetPerHour, readIdentity, startup,
			)
			if writeIdentity.Key.Principal != "" {
				ensureGitHubIdentityRuntime(
					database, budgetPerHour, writeIdentity, startup,
				)
			}
		} else {
			// Keep the ordinary side of the route present but unusable. The
			// host provider and router still need a route object for archive
			// requests; ordinary calls fail closed without borrowing the
			// archive App's identity.
			source = missingRouteTokenSource{
				host:  desc.Key.Host,
				owner: strings.TrimPrefix(desc.Key.Scope, "owner:"),
			}
			readIdentity = github.GitHubIdentity{
				Key: github.HostIdentity(desc.Key.Host),
			}
			ensureGitHubIdentityRuntime(
				database, budgetPerHour, readIdentity, startup,
			)
		}
		var archiveSource tokenauth.Source
		var archiveIdentity github.IdentityKey
		var archiveKey tokenauth.Key
		if plan.ArchiveDescriptor.Key.Host != "" {
			archiveSource = set.Upsert(plan.ArchiveDescriptor)
			archiveCtx := ctx
			if plan.GitHubOwner != "" {
				archiveCtx = tokenauth.WithGitHubOwner(archiveCtx, plan.GitHubOwner)
			}
			if _, err := archiveSource.Token(archiveCtx); err != nil {
				// Archive capacity is independent from ordinary sync. A
				// degraded startup may keep the ordinary route while leaving
				// this route absent until the next restart or reload.
				slog.Warn(
					"GitHub archive credentials unavailable; disabling archive route",
					"host", plan.ArchiveDescriptor.Key.Host,
					"scope", plan.ArchiveDescriptor.Key.Scope,
					"err", err,
				)
				archiveSource = nil
			}
		}
		if archiveSource != nil {
			archiveApp, archiveOK := activeGitHubAppCandidate(
				plan.ArchiveDescriptor, plan.GitHubOwner,
			)
			if archiveOK {
				if archiveApp.InstallationID <= 0 {
					return fmt.Errorf(
						"resolve GitHub archive identity for %s via %s: invalid installation id %d",
						plan.ArchiveDescriptor.Key.Scope,
						plan.ArchiveDescriptor.SafeString(), archiveApp.InstallationID,
					)
				}
				archiveIdentity = github.InstallationIdentity(
					plan.ArchiveDescriptor.Key.Host, archiveApp.InstallationID,
				).Key
				archiveKey = plan.ArchiveDescriptor.Key
				ensureGitHubIdentityRuntime(
					database, budgetPerHour,
					github.GitHubIdentity{Key: archiveIdentity}, startup,
				)
			}
		}
		discoveryOwner := ""
		if hasApp && strings.HasPrefix(desc.Key.Scope, "repo:") {
			discoveryOwner = app.InstallationAccount
		}
		startup.githubRoutes[desc.Key] = githubCredentialRoute{
			key:                 desc.Key,
			source:              source,
			discoveryOwner:      discoveryOwner,
			readIdentity:        readIdentity.Key,
			writeIdentity:       writeIdentity.Key,
			archiveKey:          archiveKey,
			archiveSource:       archiveSource,
			archiveReadIdentity: archiveIdentity,
		}
	}
	return nil
}

func hasExplicitGitHubFallback(cfg *config.Config, host string) bool {
	if cfg == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	// Loaded configs always carry a github_token_env value because Load
	// defaults it and Save writes it back; only a non-default name marks a
	// deliberate github.com fallback that may hold startup hostage.
	if host == platform.DefaultGitHubHost && cfg.HasExplicitGitHubTokenEnv() {
		return true
	}
	for _, configured := range cfg.Platforms {
		if strings.EqualFold(configured.Type, string(platform.KindGitHub)) &&
			strings.EqualFold(configured.Host, host) &&
			(strings.TrimSpace(configured.TokenEnv) != "" || strings.TrimSpace(configured.TokenFile) != "") {
			return true
		}
	}
	return false
}

// writeCredentialKey names the credential a route's mutations authenticate as.
// Token resolution skips App candidates under mutation auth, so the write chain
// is the descriptor's non-App candidates: two routes on different Apps that
// fall back to the same PAT must agree on it even though their read chains do
// not.
func writeCredentialKey(desc tokenauth.Descriptor) string {
	parts := make([]string, 0, len(desc.Candidates))
	seen := make(map[string]struct{}, len(desc.Candidates))
	for _, candidate := range desc.Candidates {
		if candidate.InstallationID != 0 {
			continue
		}
		part := candidate.SafeString()
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		parts = append(parts, part)
	}
	return strings.Join(parts, " -> ")
}

func buildGitHubRouteClients(startup *providerControlPlane) error {
	if startup == nil || len(startup.githubRoutes) == 0 {
		return nil
	}
	byHost := make(map[string][]*github.Route)
	discoveryRoutes := make(map[string]struct{})
	for key, configured := range startup.githubRoutes {
		readRuntime := startup.githubIdentities[configured.readIdentity.String()]
		var writeRuntime *githubIdentityRuntime
		if configured.writeIdentity.Principal != "" {
			writeRuntime = startup.githubIdentities[configured.writeIdentity.String()]
		}
		if readRuntime == nil || (configured.writeIdentity.Principal != "" && writeRuntime == nil) {
			return fmt.Errorf("create GitHub route %s: missing identity runtime", key.Scope)
		}
		clientOptions := []github.ClientOption{}
		if writeRuntime == nil {
			clientOptions = append(clientOptions, github.WithMutationsDisabled())
		} else {
			clientOptions = append(clientOptions, github.WithNotificationAccounting(
				writeRuntime.rest, writeRuntime.budget,
			))
		}
		clientOptions = append(clientOptions, github.WithQuotaAccounting(
			startup.quotaRegistry, configured.readIdentity, configured.writeIdentity,
		))
		client, err := github.NewClient(
			configured.source, key.Host, readRuntime.rest, readRuntime.budget,
			clientOptions...,
		)
		if err != nil {
			return fmt.Errorf("create GitHub route %s client: %w", key.Scope, err)
		}
		if setter, ok := client.(graphQLRateTrackerSetter); ok {
			setter.SetGraphQLRateTracker(readRuntime.graphql)
		}
		if writeRuntime != nil {
			if setter, ok := client.(writeRateTrackerSetter); ok {
				setter.SetWriteRateTracker(writeRuntime.rest)
				setter.SetWriteGraphQLRateTracker(writeRuntime.graphql)
			}
			writeBucket := github.RateBucketKey(
				string(platform.KindGitHub), key.Host,
				configured.writeIdentity.Principal,
			)
			startup.writeRateTrackers[writeBucket] = writeRuntime.rest
			startup.writeGQLRateTrackers[writeBucket] = writeRuntime.graphql
		}
		fetcher := github.NewGraphQLFetcher(
			configured.source, key.Host, readRuntime.graphql, readRuntime.budget,
			github.WithGraphQLQuotaAccounting(
				startup.quotaRegistry, configured.readIdentity,
			),
		)
		var archiveClient github.Client
		var archiveFetcher *github.GraphQLFetcher
		if configured.archiveSource != nil && configured.archiveReadIdentity.Principal != "" {
			archiveRuntime := startup.githubIdentities[configured.archiveReadIdentity.String()]
			if archiveRuntime == nil {
				return fmt.Errorf("create GitHub route %s archive client: missing identity runtime", key.Scope)
			}
			archiveClient, err = github.NewClient(
				configured.archiveSource, key.Host, archiveRuntime.rest,
				archiveRuntime.budget,
				github.WithMutationsDisabled(),
				github.WithQuotaAccounting(
					startup.quotaRegistry,
					configured.archiveReadIdentity, configured.archiveReadIdentity,
				),
			)
			if err != nil {
				return fmt.Errorf("create GitHub route %s archive client: %w", key.Scope, err)
			}
			if setter, ok := archiveClient.(graphQLRateTrackerSetter); ok {
				setter.SetGraphQLRateTracker(archiveRuntime.graphql)
			}
			archiveFetcher = github.NewGraphQLFetcher(
				configured.archiveSource, key.Host, archiveRuntime.graphql,
				archiveRuntime.budget,
				github.WithGraphQLQuotaAccounting(
					startup.quotaRegistry, configured.archiveReadIdentity,
				),
			)
		}
		var writeSnapshotClient github.Client
		if writeRuntime != nil && configured.writeIdentity != configured.readIdentity {
			writeSnapshotClient, err = github.NewClient(
				mutationTokenSource{Source: configured.source}, key.Host,
				writeRuntime.rest, nil,
			)
			if err != nil {
				return fmt.Errorf("create GitHub route %s write snapshot client: %w", key.Scope, err)
			}
			if setter, ok := writeSnapshotClient.(graphQLRateTrackerSetter); ok {
				setter.SetGraphQLRateTracker(writeRuntime.graphql)
			}
		}
		configured.client = client
		configured.fetcher = fetcher
		startup.githubRoutes[key] = configured
		owner, name := githubRouteOwnerAndName(key.Scope)
		var discoveryClient github.Client
		if configured.discoveryOwner != "" {
			discoveryKey := key.Host + "\x00" + strings.ToLower(configured.discoveryOwner)
			if _, exists := discoveryRoutes[discoveryKey]; !exists {
				discoveryRoutes[discoveryKey] = struct{}{}
				discoveryClient = client
			}
		}
		// Every route gets its own client, so the token source is what tells
		// apart many routes on one credential from independent credentials
		// that happen to resolve to the same account. The canonical source
		// string names the candidate chain rather than the route scope, so
		// owner routes sharing one PAT or App installation agree on it.
		descriptor := configured.source.Descriptor()
		credentialKey := descriptor.CanonicalSourceString()
		byHost[key.Host] = append(byHost[key.Host], &github.Route{
			Key:                github.RouteKey{Host: key.Host, Owner: owner, Name: name},
			CredentialKey:      credentialKey,
			WriteCredentialKey: writeCredentialKey(descriptor),
			Client:             client, DiscoveryClient: discoveryClient,
			WriteSnapshotClient: writeSnapshotClient,
			Fetcher:             fetcher, ReadIdentity: configured.readIdentity,
			WriteIdentity: configured.writeIdentity,
			ArchiveKey:    archiveRouteKey(configured.archiveKey, key.Host),
			ArchiveClient: archiveClient, ArchiveFetcher: archiveFetcher,
			ArchiveCredentialKey: archiveCredentialKey(configured.archiveSource),
			ArchiveReadIdentity:  configured.archiveReadIdentity,
		})
	}
	for host, routes := range byHost {
		router, err := github.NewHostRouter(host, routes...)
		if err != nil {
			return fmt.Errorf("create GitHub router for %s: %w", host, err)
		}
		client, err := github.NewRoutedClient(router)
		if err != nil {
			return fmt.Errorf("create routed GitHub client for %s: %w", host, err)
		}
		startup.githubRouters[host] = router
		startup.githubClients[host] = client
	}
	return nil
}

func githubRouteOwnerAndName(scope string) (string, string) {
	switch {
	case strings.HasPrefix(scope, "owner:"):
		return strings.TrimPrefix(scope, "owner:"), ""
	case strings.HasPrefix(scope, "repo:"):
		owner, name, _ := strings.Cut(strings.TrimPrefix(scope, "repo:"), "/")
		return owner, name
	default:
		return "", ""
	}
}

func archiveRouteOwnerAndName(scope string) (string, string) {
	return githubRouteOwnerAndName(strings.TrimPrefix(scope, "archive:"))
}

func archiveRouteKey(key tokenauth.Key, host string) github.RouteKey {
	owner, name := archiveRouteOwnerAndName(key.Scope)
	return github.RouteKey{Host: host, Owner: owner, Name: name}
}

func archiveCredentialKey(source tokenauth.Source) string {
	if source == nil {
		return ""
	}
	return source.Descriptor().CanonicalSourceString()
}

func githubCredentialPlans(cfg *config.Config) []config.ProviderTokenSource {
	plans := cfg.ProviderTokenSources()
	out := make([]config.ProviderTokenSource, 0, len(plans))
	for _, plan := range plans {
		if plan.Descriptor.Key.Platform != string(platform.KindGitHub) {
			continue
		}
		out = append(out, plan)
	}
	return out
}

type resolvedGitHubPAT struct {
	identity github.GitHubIdentity
	token    string
	// missing records that this mutation chain has no credential at all.
	// Only that verdict is cached: it is deterministic for the startup pass,
	// while a transient resolution failure must stay retryable for the next
	// route that shares the chain.
	missing error
}

func resolveGitHubPATIdentity(
	ctx context.Context,
	resolver github.IdentityResolver,
	host string,
	source tokenauth.Source,
	cache map[string]resolvedGitHubPAT,
) (resolvedGitHubPAT, error) {
	desc := source.Descriptor()
	cacheKey := strings.ToLower(strings.TrimSpace(host)) + "\x00" +
		mutationSourceIdentity(desc)
	if resolved, ok := cache[cacheKey]; ok {
		if resolved.missing != nil {
			return resolvedGitHubPAT{}, resolved.missing
		}
		return resolved, nil
	}
	identity, token, err := resolver.ResolvePAT(ctx, host, source)
	if err != nil {
		// An App-only installation gives every one of its exact repository
		// routes the same PAT-less mutation chain. Without caching the
		// verdict each route repeats the lookup, and the gh CLI candidate
		// shells out once per repository, so startup cost grows with the
		// number of repositories.
		if errors.Is(err, tokenauth.ErrMissingToken) {
			cache[cacheKey] = resolvedGitHubPAT{missing: err}
		}
		return resolvedGitHubPAT{}, err
	}
	resolved := resolvedGitHubPAT{identity: identity, token: token}
	cache[cacheKey] = resolved
	return resolved, nil
}

func mutationSourceIdentity(desc tokenauth.Descriptor) string {
	candidates := make([]tokenauth.Candidate, 0, len(desc.Candidates))
	for _, candidate := range desc.Candidates {
		if candidate.Kind != tokenauth.SourceKindGitHubApp {
			candidates = append(candidates, candidate)
		}
	}
	return (tokenauth.Descriptor{Candidates: candidates}).CanonicalSourceString()
}

func activeGitHubAppCandidate(
	desc tokenauth.Descriptor, owner string,
) (tokenauth.Candidate, bool) {
	for _, candidate := range desc.Candidates {
		if candidate.Kind != tokenauth.SourceKindGitHubApp ||
			candidate.InstallationID == 0 {
			continue
		}
		if candidate.InstallationAccount == "" ||
			(owner != "" && strings.EqualFold(
				candidate.InstallationAccount, owner,
			)) {
			return candidate, true
		}
	}
	return tokenauth.Candidate{}, false
}

func ensureGitHubIdentityRuntime(
	database *db.DB,
	budgetPerHour int,
	identity github.GitHubIdentity,
	startup *providerControlPlane,
) *githubIdentityRuntime {
	key := identity.Key.String()
	if runtime, ok := startup.githubIdentities[key]; ok {
		return runtime
	}
	runtime := &githubIdentityRuntime{
		identity: identity,
		rest: github.NewPlatformRateTracker(
			database, string(platform.KindGitHub), identity.Key.Host,
			identity.Key.Principal, "rest",
		),
		graphql: github.NewPlatformRateTracker(
			database, string(platform.KindGitHub), identity.Key.Host,
			identity.Key.Principal, "graphql",
		),
	}
	if budgetPerHour > 0 {
		runtime.budget = github.NewSyncBudgetWithEssentialReserve(budgetPerHour)
		runtime.rest.SetOnWindowReset(runtime.budget.Reset)
	}
	startup.githubIdentities[key] = runtime
	bucket := github.RateBucketKey(
		string(platform.KindGitHub), identity.Key.Host, identity.Key.Principal,
	)
	startup.ratePrincipalLabels[bucket] = identity.Label()
	startup.rateTrackers[github.RateBucketKey(
		string(platform.KindGitHub), identity.Key.Host, identity.Key.Principal,
	)] = runtime.rest
	if runtime.budget != nil {
		startup.budgets[github.RateBucketKey(
			string(platform.KindGitHub), identity.Key.Host, identity.Key.Principal,
		)] = runtime.budget
	}
	return runtime
}

func platformLabel(platformName string) string {
	if meta, ok := platform.MetadataFor(platform.Kind(platformName)); ok {
		return meta.Label
	}
	return platformName
}
