package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

type fakeGitHubIdentityResolver struct {
	byEnv map[string]github.GitHubIdentity
	err   map[string]error
	calls *atomic.Int32
}

func (r fakeGitHubIdentityResolver) ResolvePAT(
	ctx context.Context, host string, source tokenauth.Source,
) (github.GitHubIdentity, string, error) {
	if r.calls != nil {
		r.calls.Add(1)
	}
	desc := source.Descriptor()
	for _, candidate := range desc.Candidates {
		if candidate.Kind != tokenauth.SourceKindEnv {
			continue
		}
		if err := r.err[candidate.EnvName]; err != nil {
			return github.GitHubIdentity{}, "", err
		}
		if identity, ok := r.byEnv[candidate.EnvName]; ok {
			token, err := source.Token(tokenauth.WithMutationAuth(ctx))
			return identity, token, err
		}
	}
	return github.GitHubIdentity{}, "", fmt.Errorf(
		"no fake identity for %s on %s: %w",
		desc.SafeString(), host, tokenauth.ErrMissingToken,
	)
}

func TestRegisterProviderTokenSourcesDefersGitHubAppMinting(t *testing.T) {
	require := require.New(t)
	var mints atomic.Int32
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos:    []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "acme",
			RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "installation-token", time.Now().Add(time.Hour), nil
		},
	})

	sources, err := registerProviderTokenSources(cfg, set)
	require.NoError(err)
	require.NotEmpty(sources)
	require.Zero(mints.Load(), "disabled startup must not mint provider credentials")

	_, err = buildProviderControlPlane(
		t.Context(), dbtest.Open(t), cfg, set, sources,
		defaultProviderFactories(), nil,
	)
	require.NoError(err)
	require.Zero(mints.Load(), "provider construction must remain lazy")
}

func TestBuildProviderControlPlaneAddsDedicatedArchiveGitHubRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos:    []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubApps: []config.GitHubAppConfig{
			{
				Host: "github.com", AppID: 1, PrivateKeyPath: "/keys/sync.pem",
				InstallationID: 10, InstallationAccount: "acme",
				RepositorySelection: "all",
			},
			{
				Host: "github.com", AppID: 2, Role: config.GitHubAppRoleArchive,
				PrivateKeyPath: "/keys/archive.pem", InstallationID: 20,
				InstallationAccount: "acme", RepositorySelection: "all",
			},
		},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := registerProviderTokenSources(cfg, set)
	require.NoError(err)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{},
	)
	require.NoError(err)
	route := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: "github.com", Scope: "owner:acme",
	}]
	assert.Equal("installation:10", route.readIdentity.Principal)
	assert.Equal("installation:20", route.archiveReadIdentity.Principal)
	assert.NotNil(route.archiveSource)
	routes := startup.githubRouters["github.com"].Routes()
	require.Len(routes, 1)
	assert.NotNil(routes[0].ArchiveClient)
}

func TestBuildProviderControlPlaneDoesNotRequirePATForArchiveOnlyApp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos:    []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 2, Role: config.GitHubAppRoleArchive,
			PrivateKeyPath: "/keys/archive.pem", InstallationID: 20,
			InstallationAccount: "acme", RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "archive-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	startup, err := buildProviderControlPlane(
		t.Context(), dbtest.Open(t), cfg, set, sources,
		defaultProviderFactories(), fakeGitHubIdentityResolver{},
	)
	require.NoError(err)
	route := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: "github.com", Scope: "owner:acme",
	}]
	assert.Equal("installation:20", route.archiveReadIdentity.Principal)
	assert.Equal("github.com", route.readIdentity.Host)
	assert.Empty(route.writeIdentity.Principal)
	assert.NotNil(startup.githubClients["github.com"])
}

func TestCollectProviderTokenSourcesDegradedKeepsOrdinaryHostWhenArchiveFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("ORDINARY_PAT", "ordinary-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos:    []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "acme", TokenEnv: "ORDINARY_PAT",
		}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 2, Role: config.GitHubAppRoleArchive,
			PrivateKeyPath: "/keys/archive.pem", InstallationID: 20,
			InstallationAccount: "acme", RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "", time.Time{}, errors.New("archive App unavailable")
		},
	})
	sources, err := collectProviderTokenSourcesDegraded(t.Context(), cfg, set)
	require.NoError(err)
	require.Contains(sources, providerHostKey("github", "github.com"))
	_, err = sources[providerHostKey("github", "github.com")].Token(t.Context())
	require.NoError(err)
	startup, err := buildProviderControlPlaneOrDegraded(
		t.Context(), dbtest.Open(t), cfg, set, sources,
		defaultProviderFactories(), tokenGitHubIdentityResolver{
			"ordinary-token": {Key: github.IdentityKey{
				Host: "github.com", Principal: "user:7",
			}},
		},
	)
	require.NoError(err)
	assert.Len(startup.registry.Providers(), 1)
	route := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: "github.com", Scope: "owner:acme",
	}]
	assert.Empty(route.archiveReadIdentity.Principal,
		"a failed archive credential must disable only its archive route")
}

func TestBuildProviderControlPlaneRetainsVerifiedPATForExactRoutes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("SHARED_PAT", "shared-secret")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos: []config.Repo{
			{Owner: "org-a", Name: "one", TokenEnv: "SHARED_PAT"},
			{Owner: "org-a", Name: "two", TokenEnv: "SHARED_PAT"},
		},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	var identityLookups atomic.Int32
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"SHARED_PAT": {
					Key: github.IdentityKey{Host: "github.com", Principal: "user:123"},
				},
			},
			calls: &identityLookups,
		},
	)
	require.NoError(err)
	startupLookups := identityLookups.Load()
	assert.Positive(startupLookups)
	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)

	for _, repo := range []string{"one", "two"} {
		source := gitRoutes.SourceForRepo("github", "github.com", "org-a", repo)
		require.NotNil(source)
		token, tokenErr := source.Token(t.Context())
		require.NoError(tokenErr)
		assert.Equal("shared-secret", token)
	}
	assert.Equal(startupLookups, identityLookups.Load(),
		"the startup-verified PAT must not be resolved again on each exact route's first request")
}

type tokenGitHubIdentityResolver map[string]github.GitHubIdentity

func gitRoutesForProviderControlPlaneTest(
	t *testing.T,
	cfg *config.Config,
	set *tokenauth.SourceSet,
	control *providerControlPlane,
) *gitStartup {
	t.Helper()
	routes, err := buildGitStartup(cfg, set)
	require.NoError(t, err)
	routes.ApplyProviderControlPlane(control)
	return &routes
}

func (r tokenGitHubIdentityResolver) ResolvePAT(
	ctx context.Context, host string, source tokenauth.Source,
) (github.GitHubIdentity, string, error) {
	token, err := source.Token(tokenauth.WithMutationAuth(ctx))
	if err != nil {
		return github.GitHubIdentity{}, "", err
	}
	if identity, ok := r[token]; ok {
		return identity, token, nil
	}
	return github.GitHubIdentity{}, "", fmt.Errorf(
		"no fake identity for token on %s: %w", host, tokenauth.ErrMissingToken,
	)
}

func TestBuildProviderControlPlaneDeduplicatesGitHubIdentityRuntimes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	for _, env := range []string{"PAT_A", "PAT_B", "PAT_C", "APP_WRITE_PAT"} {
		t.Setenv(env, env+"-secret")
	}

	cfg := &config.Config{
		SyncBudgetPerHour: 200,
		SyncInterval:      "5m",
		Host:              "127.0.0.1",
		Port:              8091,
		BasePath:          "/",
		Activity: config.Activity{
			ViewMode: "flat", TimeRange: "7d",
		},
		Repos: []config.Repo{
			{Owner: "org-a", Name: "one"},
			{Owner: "org-b", Name: "two"},
			{Owner: "org-c", Name: "three"},
			{Owner: "org-d", Name: "four"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{
			{Host: "github.com", Owner: "org-a", TokenEnv: "PAT_A"},
			{Host: "github.com", Owner: "org-b", TokenEnv: "PAT_B"},
			{Host: "github.com", Owner: "org-c", TokenEnv: "PAT_C"},
			{Host: "github.com", Owner: "org-d", TokenEnv: "APP_WRITE_PAT"},
		},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "org-d",
			RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())

	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	resolver := fakeGitHubIdentityResolver{byEnv: map[string]github.GitHubIdentity{
		"PAT_A":         {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
		"PAT_B":         {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
		"PAT_C":         {Key: github.IdentityKey{Host: "github.com", Principal: "user:456"}},
		"APP_WRITE_PAT": {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
	}}

	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(), resolver,
	)
	require.NoError(err)

	routeA := startup.githubRoutes[tokenauth.Key{Platform: "github", Host: "github.com", Scope: "owner:org-a"}]
	routeB := startup.githubRoutes[tokenauth.Key{Platform: "github", Host: "github.com", Scope: "owner:org-b"}]
	routeC := startup.githubRoutes[tokenauth.Key{Platform: "github", Host: "github.com", Scope: "owner:org-c"}]
	routeD := startup.githubRoutes[tokenauth.Key{Platform: "github", Host: "github.com", Scope: "owner:org-d"}]

	assert.Equal("user:123", routeA.readIdentity.Principal)
	assert.Equal("user:123", routeB.readIdentity.Principal)
	assert.Equal("user:456", routeC.readIdentity.Principal)
	assert.Equal("installation:789", routeD.readIdentity.Principal)
	assert.Equal("user:123", routeD.writeIdentity.Principal)

	runtimeA := startup.githubIdentities[routeA.readIdentity.String()]
	runtimeB := startup.githubIdentities[routeB.readIdentity.String()]
	runtimeC := startup.githubIdentities[routeC.readIdentity.String()]
	runtimeDRead := startup.githubIdentities[routeD.readIdentity.String()]
	runtimeDWrite := startup.githubIdentities[routeD.writeIdentity.String()]
	require.NotNil(runtimeA)
	require.NotNil(runtimeB)
	require.NotNil(runtimeC)
	require.NotNil(runtimeDRead)
	require.NotNil(runtimeDWrite)
	assert.Same(runtimeA.budget, runtimeB.budget)
	assert.Same(runtimeA.rest, runtimeB.rest)
	assert.NotSame(runtimeA.budget, runtimeC.budget)
	assert.Same(runtimeA.budget, runtimeDWrite.budget)
	assert.NotSame(runtimeDRead.budget, runtimeDWrite.budget)
	writeBucket := github.RateBucketKey("github", "github.com", "user:123")
	assert.Same(runtimeDWrite.rest, startup.writeRateTrackers[writeBucket])
	assert.Same(runtimeDWrite.graphql, startup.writeGQLRateTrackers[writeBucket])
	assert.NotContains(startup.rateTrackers, github.RateBucketKey("github", "github.com", "host"))
	assert.NotContains(startup.budgets, github.RateBucketKey("github", "github.com", "host"))
	assert.Empty(startup.fetchers, "routed GitHub hosts must use route fetchers only")
	assert.NotSame(routeA.client, routeB.client)
	assert.Same(runtimeA.graphql, routeA.fetcher.RateTracker())
	assert.Same(runtimeB.graphql, routeB.fetcher.RateTracker())
	assert.Same(runtimeDRead.graphql, routeD.fetcher.RateTracker())
	router := startup.githubRouters["github.com"]
	require.NotNil(router)
	routedA, err := router.RouteForRepo("org-a", "one")
	require.NoError(err)
	routedB, err := router.RouteForRepo("org-b", "two")
	require.NoError(err)
	assert.Same(routeA.fetcher, routedA.Fetcher)
	assert.Same(routeB.fetcher, routedB.Fetcher)

	routed, ok := startup.githubClients["github.com"].(*github.RoutedClient)
	require.True(ok)
	assert.NotNil(routed)

	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)
	gitSource := gitRoutes.SourceForRepo("github", "github.com", "org-d", "four")
	require.NotNil(gitSource)
	gitToken, err := gitSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("APP_WRITE_PAT-secret", gitToken,
		"managed Git must use the user PAT, never the App installation token")
}

// Rate-limit snapshot refresh deduplicates by the route's credential key, so
// production route construction has to populate it: owner routes sharing one
// PAT must agree on it even though each route gets its own client and its own
// scope, while a route on an independent credential must not.
func TestProviderControlPlaneGivesRoutesSharingOnePATOneCredentialKey(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("SHARED_PAT", "shared-token")
	t.Setenv("DEFAULT_PAT", "fallback-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity:       config.Activity{ViewMode: "flat", TimeRange: "7d"},
		GitHubTokenEnv: "DEFAULT_PAT",
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{
			{Host: "github.com", Owner: "org-a", TokenEnv: "SHARED_PAT"},
			{Host: "github.com", Owner: "org-b", TokenEnv: "SHARED_PAT"},
		},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{byEnv: map[string]github.GitHubIdentity{
			"SHARED_PAT":  {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
			"DEFAULT_PAT": {Key: github.IdentityKey{Host: "github.com", Principal: "user:999"}},
		}},
	)
	require.NoError(err)

	router := startup.githubRouters["github.com"]
	require.NotNil(router)
	orgA, err := router.RouteForRepo("org-a", "first")
	require.NoError(err)
	orgB, err := router.RouteForRepo("org-b", "second")
	require.NoError(err)
	fallback, err := router.RouteForRepo("unconfigured", "third")
	require.NoError(err)

	assert.NotEmpty(orgA.CredentialKey,
		"route construction must name the credential behind each client")
	assert.Equal(orgA.CredentialKey, orgB.CredentialKey,
		"owner routes sharing one PAT share one credential")
	assert.NotEqual(orgA.CredentialKey, fallback.CredentialKey,
		"an independent credential must stay distinguishable")
	assert.NotSame(orgA.Client, orgB.Client,
		"each route still gets its own client, which is why the key is needed")
}

// Mutations skip App candidates, so two owner routes on different App
// installations that fall back to the same PAT write as one credential even
// though their read chains differ. Snapshot refresh dedupes write buckets on
// the write key, so the read key cannot stand in for it.
func TestProviderControlPlaneSeparatesWriteCredentialKeyFromReadChain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("SHARED_PAT", "shared-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity:       config.Activity{ViewMode: "flat", TimeRange: "7d"},
		GitHubTokenEnv: "SHARED_PAT",
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{
			{Host: "github.com", Owner: "org-a", TokenEnv: "SHARED_PAT"},
			{Host: "github.com", Owner: "org-b", TokenEnv: "SHARED_PAT"},
		},
		GitHubApps: []config.GitHubAppConfig{
			{
				Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/a.pem",
				InstallationID: 701, InstallationAccount: "org-a",
				RepositorySelection: "all",
			},
			{
				Host: "github.com", AppID: 8, PrivateKeyPath: "/keys/b.pem",
				InstallationID: 802, InstallationAccount: "org-b",
				RepositorySelection: "all",
			},
		},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{byEnv: map[string]github.GitHubIdentity{
			"SHARED_PAT": {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
		}},
	)
	require.NoError(err)

	router := startup.githubRouters["github.com"]
	require.NotNil(router)
	orgA, err := router.RouteForRepo("org-a", "first")
	require.NoError(err)
	orgB, err := router.RouteForRepo("org-b", "second")
	require.NoError(err)

	assert.NotEqual(orgA.CredentialKey, orgB.CredentialKey,
		"different App installations are different read credentials")
	assert.NotEmpty(orgA.WriteCredentialKey)
	assert.Equal(orgA.WriteCredentialKey, orgB.WriteCredentialKey,
		"both routes mutate through the same PAT")
	assert.NotContains(orgA.WriteCredentialKey, "github_app",
		"the write chain must not carry App candidates mutations never use")
}

func TestBuildProviderControlPlaneRoutesUntrackedOwnerAndKeepsFallbackUnscoped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ORG_A_PAT", "org-a-token")
	t.Setenv("DEFAULT_PAT", "fallback-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity:       config.Activity{ViewMode: "flat", TimeRange: "7d"},
		GitHubTokenEnv: "DEFAULT_PAT",
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "ORG_A_PAT",
		}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "org-a",
			RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{byEnv: map[string]github.GitHubIdentity{
			"ORG_A_PAT":   {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
			"DEFAULT_PAT": {Key: github.IdentityKey{Host: "github.com", Principal: "user:999"}},
		}},
	)
	require.NoError(err)
	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)

	source := gitRoutes.SourceForRepo(
		"github", "github.com", "org-a", "first-repo",
	)
	require.NotNil(source)
	token, err := source.Token(t.Context())
	require.NoError(err)
	assert.Equal("org-a-token", token)

	nonGitHub := gitRoutes.SourceForRepo(
		"forgejo", "github.com", "org-a", "first-repo",
	)
	assert.Nil(nonGitHub, "an unconfigured provider must not borrow another provider's credential")

	fallback := gitRoutes.FallbackSource("github.com")
	require.NotNil(fallback)
	assert.NotContains(fallback.Descriptor().CanonicalSourceString(), "ORG_A_PAT")
	assert.NotContains(fallback.Descriptor().CanonicalSourceString(), "github_app")
	fallbackToken, err := fallback.Token(t.Context())
	require.NoError(err)
	assert.Equal("fallback-token", fallbackToken)

	router := startup.githubRouters["github.com"]
	require.NotNil(router)
	fallbackRoute, err := router.RouteForRepo("unconfigured", "repo")
	require.NoError(err)
	assert.Equal("user:999", fallbackRoute.ReadIdentity.Principal)
}

// TestProviderControlPlaneKeepsSameHostGitCredentialsProviderScoped drives the
// reachable shared-host configuration — scoped GitHub owner routes plus
// another provider on the same hostname — through config.Load and
// buildProviderControlPlane, then asserts managed Git credential selection through
// the gitclone.RouteResolver interface that gitclone.Manager consults for
// every networked operation.
func TestProviderControlPlaneKeepsSameHostGitCredentialsProviderScoped(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("ORG_A_PAT", "org-a-token")
	t.Setenv("FORGEJO_PAT", "forgejo-secret")
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"9.0.0"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(api.URL, "https://")
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, fmt.Appendf(nil, `
[[platforms]]
type = "forgejo"
host = %[1]q
token_env = "FORGEJO_PAT"

[[repos]]
platform = "forgejo"
platform_host = %[1]q
owner = "acme"
name = "widget"

[[repos]]
platform = "github"
platform_host = %[1]q
owner = "org-a"
name = "one"

[[github_owner_tokens]]
host = %[1]q
owner = "org-a"
token_env = "ORG_A_PAT"
`, host), 0o600))
	cfg, err := config.Load(path)
	require.NoError(err)
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	database := dbtest.Open(t)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"ORG_A_PAT": {
					Key: github.IdentityKey{Host: host, Principal: "user:123"},
				},
			},
		},
	)
	require.NoError(err)

	var routes gitclone.RouteResolver = gitRoutesForProviderControlPlaneTest(
		t, cfg, set, &startup,
	)
	forgejoSource := routes.SourceForRepo("forgejo", host, "acme", "widget")
	require.NotNil(forgejoSource)
	forgejoToken, err := forgejoSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("forgejo-secret", forgejoToken)

	githubSource := routes.SourceForRepo("github", host, "org-a", "one")
	require.NotNil(githubSource)
	githubToken, err := githubSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("org-a-token", githubToken,
		"the scoped GitHub route must not borrow the forgejo host token")

	// Fail closed means a source that reports the missing route, not a nil
	// source: managed Git reads nil as permission to run git with no
	// credential, which succeeds against any public repository.
	unmatched := routes.SourceForRepo("github", host, "unmatched", "repo")
	require.NotNil(unmatched,
		"a routed GitHub host must never hand managed Git a credential-free runner")
	_, err = unmatched.Token(t.Context())
	var missing *github.MissingRouteError
	require.ErrorAs(err, &missing,
		"a routed GitHub host without a matching route must fail closed")
	assert.Equal("unmatched", missing.Owner)
}

// TestProviderControlPlaneDisablesAmbiguousOwnerlessFallbackOnSharedHost pins the
// shared-host disagreement rule end to end: providers keep their own
// credentials for repository-scoped operations, while the ownerless host
// fallback fails closed instead of exposing the GitHub credential for work
// that may belong to another provider.
func TestProviderControlPlaneDisablesAmbiguousOwnerlessFallbackOnSharedHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("GITHUB_HOST_PAT", "github-secret")
	t.Setenv("FORGEJO_PAT", "forgejo-secret")
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"version":"9.0.0"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(api.URL, "https://")
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, fmt.Appendf(nil, `
[[platforms]]
type = "github"
host = %[1]q
token_env = "GITHUB_HOST_PAT"

[[platforms]]
type = "forgejo"
host = %[1]q
token_env = "FORGEJO_PAT"
`, host), 0o600))
	cfg, err := config.Load(path)
	require.NoError(err)
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	database := dbtest.Open(t)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"GITHUB_HOST_PAT": {
					Key: github.IdentityKey{Host: host, Principal: "user:777"},
				},
			},
		},
	)
	require.NoError(err)

	var routes gitclone.RouteResolver = gitRoutesForProviderControlPlaneTest(
		t, cfg, set, &startup,
	)
	forgejoSource := routes.SourceForRepo("forgejo", host, "acme", "widget")
	require.NotNil(forgejoSource)
	forgejoToken, err := forgejoSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("forgejo-secret", forgejoToken)

	githubSource := routes.SourceForRepo("github", host, "acme", "widget")
	require.NotNil(githubSource)
	githubToken, err := githubSource.Token(t.Context())
	require.NoError(err)
	assert.Equal("github-secret", githubToken)

	fallback := routes.FallbackSource(host)
	require.NotNil(fallback)
	_, err = fallback.Token(t.Context())
	require.ErrorIs(err, tokenauth.ErrMissingToken,
		"an ambiguous shared-host ownerless fallback must fail closed")
}

func TestSelectedGitHubAppSupportsOwnerPreviewWithoutPATFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var installationAuth string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v3/installation/repositories" {
			installationAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"repositories":[{
				"id":101,"name":"covered","full_name":"acme/covered",
				"owner":{"login":"acme"},"private":true,"archived":false,"fork":false
			}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(api.URL, "https://")

	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		GitHubApps: []config.GitHubAppConfig{{
			Host: host, AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "acme",
			RepositorySelection: "selected", SelectedRepos: []string{"acme/covered"},
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	database := dbtest.Open(t)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{},
	)
	require.NoError(err)
	_, err = startup.githubRouters[host].RouteForRepo("acme", "uncovered")
	require.Error(err, "the discovery client must not become a repository fallback")
	syncer := github.NewSyncerWithRegistry(
		startup.registry, database, nil, nil, time.Minute,
		startup.rateTrackers, startup.budgets,
	)
	t.Cleanup(syncer.Stop)
	syncer.SetGitHubRouters(startup.githubRouters)
	srv := server.NewWithConfig(database, syncer, nil, nil, cfg, t.TempDir()+"/config.toml", server.ServerOptions{
		TokenSources: set, HostCheckAllowLoopbackAnyPort: true,
	})

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/repos/preview",
		strings.NewReader(fmt.Sprintf(
			`{"provider":"github","host":%q,"owner":"acme","pattern":"*"}`,
			host,
		)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8091"
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var response struct {
		Repos []struct {
			Name string `json:"name"`
		} `json:"repos"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&response))
	require.Len(response.Repos, 1)
	assert.Equal("covered", response.Repos[0].Name)
	assert.Equal("Bearer app-token", installationAuth)
}

func TestBuildProviderControlPlaneProbesImplicitFallbackBestEffort(t *testing.T) {
	// Load from disk: Load defaults github_token_env to the built-in env
	// name, so a config that never mentions it is an implicit fallback that
	// must be probed best-effort: kept when its token resolves, skipped with
	// a warning (never failing startup) when it does not.
	loadConfig := func(t *testing.T) *config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(`
[[repos]]
owner = "org-a"
name = "one"

[[github_owner_tokens]]
host = "github.com"
owner = "org-a"
token_env = "ORG_A_PAT"
`), 0o600))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		return cfg
	}
	fallbackKey := tokenauth.Key{Platform: "github", Host: "github.com"}
	buildStartup := func(
		t *testing.T, resolver fakeGitHubIdentityResolver,
	) (providerControlPlane, error) {
		t.Helper()
		cfg := loadConfig(t)
		set := tokenauth.NewSourceSet(tokenauth.Options{
			GitHubCLI: func(context.Context, string) (string, error) {
				return "", tokenauth.ErrMissingToken
			},
		})
		sources, err := collectProviderTokenSources(t.Context(), cfg, set)
		require.NoError(t, err)
		return buildProviderControlPlane(
			t.Context(), dbtest.Open(t), cfg, set, sources,
			defaultProviderFactories(), resolver,
		)
	}

	t.Run("invalid token skips fallback without failing startup", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		t.Setenv("ORG_A_PAT", "org-a-token")
		t.Setenv("KENN_FORGE_GITHUB_TOKEN", "invalid-implicit-fallback-token")
		var identityLookups atomic.Int32

		startup, err := buildStartup(t, fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"ORG_A_PAT": {
					Key: github.IdentityKey{Host: "github.com", Principal: "user:123"},
				},
			},
			err: map[string]error{
				"KENN_FORGE_GITHUB_TOKEN": errors.New("401 bad credentials"),
			},
			calls: &identityLookups,
		})
		require.NoError(err)
		assert.Equal(int32(2), identityLookups.Load(),
			"the implicit fallback must be probed, not silently dropped")
		assert.NotContains(startup.githubRoutes, fallbackKey)
	})

	t.Run("valid token keeps ownerless APIs routed", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		t.Setenv("ORG_A_PAT", "org-a-token")
		t.Setenv("KENN_FORGE_GITHUB_TOKEN", "valid-implicit-fallback-token")
		var identityLookups atomic.Int32

		startup, err := buildStartup(t, fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"ORG_A_PAT": {
					Key: github.IdentityKey{Host: "github.com", Principal: "user:123"},
				},
				"KENN_FORGE_GITHUB_TOKEN": {
					Key: github.IdentityKey{Host: "github.com", Principal: "user:555"},
				},
			},
			calls: &identityLookups,
		})
		require.NoError(err)
		assert.Equal(int32(2), identityLookups.Load())
		assert.Contains(startup.githubRoutes, fallbackKey)
	})
}

func TestProductionStartupRoutesTwoOwnersThroughSyncAndMutationAPI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("ORG_A_PAT", "token-a")
	t.Setenv("ORG_B_PAT", "token-b")

	var authMu sync.Mutex
	authByCall := make(map[string]string)
	record := func(key string, r *http.Request) {
		authMu.Lock()
		defer authMu.Unlock()
		if _, exists := authByCall[key]; !exists {
			authByCall[key] = r.Header.Get("Authorization")
		}
	}
	repoIDs := map[string]int64{"org-a/one": 101, "org-b/two": 202}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "2000000000")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/user":
			record("identity:"+r.Header.Get("Authorization"), r)
			switch r.Header.Get("Authorization") {
			case "Bearer token-a":
				_, _ = io.WriteString(w, `{"id":11,"login":"owner-a-user"}`)
			case "Bearer token-b":
				_, _ = io.WriteString(w, `{"id":22,"login":"owner-b-user"}`)
			default:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"message":"bad credentials"}`)
			}
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/rate_limit":
			_, _ = io.WriteString(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":2000000000}}}`)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/api/graphql":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"force REST fallback"}`)
			return
		}

		if !strings.HasPrefix(r.URL.Path, "/api/v3/repos/") {
			http.NotFound(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v3/repos/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		fullName := parts[0] + "/" + parts[1]
		repoID, ok := repoIDs[fullName]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 2 && r.Method == http.MethodGet {
			record("sync:repo:"+fullName, r)
			_, _ = fmt.Fprintf(w, `{
				"id":%d,"node_id":%q,"name":%q,"full_name":%q,
				"owner":{"login":%q},"default_branch":"main",
				"html_url":"https://example.invalid/%s",
				"clone_url":"https://example.invalid/%s.git",
				"permissions":{"push":true}
			}`, repoID, fmt.Sprintf("R_%d", repoID), parts[1], fullName, parts[0], fullName, fullName)
			return
		}
		if len(parts) == 3 && parts[2] == "pulls" && r.Method == http.MethodGet {
			record("sync:pulls:"+fullName, r)
			_, _ = fmt.Fprintf(w, `[{
				"id":%d,"number":1,"state":"open","title":%q,
				"html_url":"https://example.invalid/%s/pull/1",
				"user":{"login":"author"},"draft":false,
				"created_at":"2026-07-17T12:00:00Z","updated_at":"2026-07-17T12:00:00Z",
				"head":{"sha":%q,"ref":"feature","repo":{"id":%d,"full_name":%q}},
				"base":{"sha":%q,"ref":"main","repo":{"id":%d,"full_name":%q}}
			}]`, repoID*10+1, fullName+" PR", fullName, "head-"+parts[0], repoID, fullName, "base-"+parts[0], repoID, fullName)
			return
		}
		if len(parts) == 5 && parts[2] == "issues" && parts[3] == "1" &&
			parts[4] == "comments" && r.Method == http.MethodPost {
			record("write:comment:"+fullName, r)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{
				"id":%d,"body":"routed comment","user":{"login":"maintainer"},
				"created_at":"2026-07-17T12:01:00Z",
				"html_url":"https://example.invalid/%s/pull/1#issuecomment-1"
			}`, repoID*100, fullName)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(api.Close)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(api.URL, "https://")

	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos: []config.Repo{
			{Platform: "github", PlatformHost: host, Owner: "org-a", Name: "one"},
			{Platform: "github", PlatformHost: host, Owner: "org-b", Name: "two"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{
			{Host: host, Owner: "org-a", TokenEnv: "ORG_A_PAT"},
			{Host: host, Owner: "org-b", TokenEnv: "ORG_B_PAT"},
		},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	database := dbtest.Open(t)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		github.HTTPIdentityResolver{},
	)
	require.NoError(err)

	// Credential routing must not narrow archive support: the registry
	// provider wraps the routed client, and wrapping once silently dropped
	// inventory pagination and with it every archive capability.
	caps, err := startup.registry.Capabilities(platform.KindGitHub, host)
	require.NoError(err)
	assert.Equal(platform.ArchiveCapabilities{
		HistoricalIssues: true, HistoricalMergeRequests: true,
		OrdinaryComments: true, SubmittedReviews: true, InlineReviewComments: true,
	}, caps.Archive)

	repos := []github.RepoRef{
		{Platform: "github", PlatformHost: host, Owner: "org-a", Name: "one"},
		{Platform: "github", PlatformHost: host, Owner: "org-b", Name: "two"},
	}
	syncer := github.NewSyncerWithRegistry(
		startup.registry, database, nil, repos, time.Minute,
		startup.rateTrackers, startup.budgets,
	)
	t.Cleanup(syncer.Stop)
	syncer.SetGitHubRouters(startup.githubRouters)
	syncer.SetWriteRateTrackers(startup.writeRateTrackers)
	syncer.SetWriteGQLRateTrackers(startup.writeGQLRateTrackers)
	srv := server.New(database, syncer, nil, "/", cfg, server.ServerOptions{
		TokenSources: set, HostCheckAllowLoopbackAnyPort: true,
	})
	forge := httptest.NewServer(srv)
	t.Cleanup(forge.Close)

	syncReq, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, forge.URL+"/api/v1/sync", nil,
	)
	require.NoError(err)
	syncReq.Header.Set("Content-Type", "application/json")
	syncResp, err := forge.Client().Do(syncReq)
	require.NoError(err)
	syncBody, err := io.ReadAll(syncResp.Body)
	require.NoError(err)
	require.NoError(syncResp.Body.Close())
	require.Equal(http.StatusAccepted, syncResp.StatusCode, string(syncBody))

	require.Eventually(func() bool {
		for _, repo := range repos {
			row, rowErr := database.GetMergeRequest(
				t.Context(), "github", host, repo.Owner, repo.Name, 1,
			)
			if rowErr != nil || row == nil || row.Title != repo.Owner+"/"+repo.Name+" PR" {
				return false
			}
		}
		return true
	}, 30*time.Second, 20*time.Millisecond)

	for _, repo := range repos {
		url := fmt.Sprintf(
			"%s/api/v1/host/%s/pulls/gh/%s/%s/1/comments",
			forge.URL, host, repo.Owner, repo.Name,
		)
		commentReq, reqErr := http.NewRequestWithContext(
			t.Context(), http.MethodPost, url,
			strings.NewReader(`{"body":"routed comment"}`),
		)
		require.NoError(reqErr)
		commentReq.Header.Set("Content-Type", "application/json")
		commentResp, reqErr := forge.Client().Do(commentReq)
		require.NoError(reqErr)
		commentBody, readErr := io.ReadAll(commentResp.Body)
		require.NoError(readErr)
		require.NoError(commentResp.Body.Close())
		require.Equal(http.StatusCreated, commentResp.StatusCode, string(commentBody))
	}

	authMu.Lock()
	defer authMu.Unlock()
	assert.Equal("Bearer token-a", authByCall["sync:pulls:org-a/one"])
	assert.Equal("Bearer token-b", authByCall["sync:pulls:org-b/two"])
	assert.Equal("Bearer token-a", authByCall["write:comment:org-a/one"])
	assert.Equal("Bearer token-b", authByCall["write:comment:org-b/two"])
}

func TestProductionStartupRoutesExposeRotatedPATThroughRepoAPI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ACME_PAT", "writer-a")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos: []config.Repo{
			{Owner: "acme", Name: "covered"},
			{Owner: "acme", Name: "uncovered"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "acme", TokenEnv: "ACME_PAT",
		}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "acme",
			RepositorySelection: "selected", SelectedRepos: []string{"acme/covered"},
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	resolver := tokenGitHubIdentityResolver{
		"writer-a": {Key: github.IdentityKey{Host: "github.com", Principal: "user:123"}},
		"writer-b": {Key: github.IdentityKey{Host: "github.com", Principal: "user:456"}},
	}
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(), resolver,
	)
	require.NoError(err)

	coveredRoute := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: "github.com", Scope: "repo:acme/covered",
	}]
	uncoveredRoute := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: "github.com", Scope: "owner:acme",
	}]
	assert.Equal("installation:789", coveredRoute.readIdentity.Principal)
	assert.Equal("user:123", coveredRoute.writeIdentity.Principal)
	assert.Equal("user:123", uncoveredRoute.readIdentity.Principal)
	assert.Equal("user:123", uncoveredRoute.writeIdentity.Principal)

	repos := []github.RepoRef{
		{Owner: "acme", Name: "covered", PlatformHost: "github.com", PlatformExternalID: "repo-acme-covered"},
		{Owner: "acme", Name: "uncovered", PlatformHost: "github.com", PlatformExternalID: "repo-acme-uncovered"},
	}
	for _, repo := range repos {
		_, err := database.UpsertRepo(
			t.Context(), db.RepoIdentity{
				Platform: "github", PlatformHost: repo.PlatformHost,
				PlatformRepoID: repo.PlatformExternalID, Owner: repo.Owner, Name: repo.Name,
			},
		)
		require.NoError(err)
	}
	syncer := github.NewSyncerWithRegistry(
		startup.registry, database, nil, repos, time.Minute,
		startup.rateTrackers, startup.budgets,
	)
	t.Cleanup(syncer.Stop)
	syncer.SetGitHubRouters(startup.githubRouters)
	syncer.SetWriteRateTrackers(startup.writeRateTrackers)
	syncer.SetWriteGQLRateTrackers(startup.writeGQLRateTrackers)
	srv := server.New(database, syncer, nil, "/", cfg, server.ServerOptions{
		TokenSources: set, HostCheckAllowLoopbackAnyPort: true,
	})
	httpServer := httptest.NewServer(srv)
	t.Cleanup(httpServer.Close)

	// The bounded routes keep user:123 until restart, so rotating the live PAT
	// to user:456 must disable writes for both the App-covered exact route and
	// the PAT-backed owner route through the real repository API.
	t.Setenv("ACME_PAT", "writer-b")
	for _, name := range []string{"covered", "uncovered"} {
		resp, err := http.Get(httpServer.URL + "/api/v1/repo/github/acme/" + name)
		require.NoError(err)
		var body struct {
			Operations struct {
				AddComment struct {
					Code string `json:"code"`
				} `json:"add_comment"`
			} `json:"operations"`
		}
		require.NoError(json.NewDecoder(resp.Body).Decode(&body))
		require.NoError(resp.Body.Close())
		assert.Equal(http.StatusOK, resp.StatusCode)
		assert.Equal("write_credential_error", body.Operations.AddComment.Code)
	}
}

func TestBuildProviderControlPlaneAllowsAppOnlyReadRouteButRequiresRestartForManagedGit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("LATE_PAT", "")
	var requestCount atomic.Int32
	auth := make(chan string, 4)
	gitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if username, password, ok := r.BasicAuth(); ok {
			select {
			case auth <- username + "\x00" + password:
			default:
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer gitServer.Close()
	host := gitServer.Listener.Addr().String()
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Platforms: []config.PlatformConfig{{
			Type: "github", Host: host, TokenEnv: "LATE_PAT",
		}},
		Repos: []config.Repo{{
			Platform: "github", PlatformHost: host, Owner: "org-app", Name: "one",
		}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: host, AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "org-app",
			RepositorySelection: "all",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{},
	)
	require.NoError(err)
	route := startup.githubRoutes[tokenauth.Key{
		Platform: "github", Host: host, Scope: "owner:org-app",
	}]
	assert.Equal("installation:789", route.readIdentity.Principal)
	assert.Empty(route.writeIdentity.Principal)
	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)

	t.Setenv("LATE_PAT", "appeared-after-startup")
	manager := gitclone.New(t.TempDir(), gitRoutes)
	_, err = manager.RunGitForRepo(
		t.Context(), "github", host, "org-app", "one", "",
		"ls-remote", gitServer.URL+"/org-app/one.git",
	)
	require.ErrorContains(err, github.ErrMissingWriteIdentity.Error(),
		"managed Git must wait for restart to bind a newly available PAT identity")
	assert.Zero(requestCount.Load(), "managed Git must not contact the remote before restart")

	restartedSet := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	restartedSources, err := collectProviderTokenSources(t.Context(), cfg, restartedSet)
	require.NoError(err)
	restarted, err := buildProviderControlPlane(
		t.Context(), database, cfg, restartedSet, restartedSources,
		defaultProviderFactories(), tokenGitHubIdentityResolver{
			"appeared-after-startup": {
				Key: github.IdentityKey{Host: host, Principal: "user:123"},
			},
		},
	)
	require.NoError(err)
	restartedRoutes := gitRoutesForProviderControlPlaneTest(
		t, cfg, restartedSet, &restarted,
	)

	restartedManager := gitclone.New(t.TempDir(), restartedRoutes)
	_, err = restartedManager.RunGitForRepo(
		t.Context(), "github", host, "org-app", "one", "",
		"ls-remote", gitServer.URL+"/org-app/one.git",
	)
	require.Error(err, "the controlled endpoint rejects the authenticated fetch")
	assert.NotContains(err.Error(), github.ErrMissingWriteIdentity.Error())
	assert.Positive(requestCount.Load())
	select {
	case got := <-auth:
		assert.Equal("x-access-token\x00appeared-after-startup", got)
	default:
		require.Fail("managed Git did not send Basic authentication after restart")
	}
}

func TestBuildProviderControlPlaneReportsSafeGitHubIdentityResolutionFailure(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	t.Setenv("ORG_A_TOKEN", "super-secret-token")
	cfg := &config.Config{
		SyncInterval: "5m",
		Host:         "127.0.0.1",
		Port:         8091,
		BasePath:     "/",
		Activity: config.Activity{
			ViewMode: "flat", TimeRange: "7d",
		},
		Repos: []config.Repo{{Owner: "org-a", Name: "one"}},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "ORG_A_TOKEN",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	_, err = buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{err: map[string]error{
			"ORG_A_TOKEN": fmt.Errorf("identity lookup failed"),
		}},
	)
	require.Error(err)
	assert.Contains(t, err.Error(), "env:ORG_A_TOKEN")
	assert.NotContains(t, err.Error(), "super-secret-token")
}

func TestBuildProviderControlPlaneOrDegradedKeepsServerBootableWhenGitHubUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ORG_A_TOKEN", "org-a-token")
	cfg := &config.Config{
		SyncInterval: "5m",
		Host:         "127.0.0.1",
		Port:         8091,
		BasePath:     "/",
		Activity:     config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos:        []config.Repo{{Owner: "org-a", Name: "one"}},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "ORG_A_TOKEN",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlaneOrDegraded(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{err: map[string]error{
			"ORG_A_TOKEN": errors.New("GitHub API unavailable: 503"),
		}},
	)
	require.NoError(err)
	assert.Empty(startup.registry.Providers())
}

func TestCollectProviderTokenSourcesDegradedExcludesFailedHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("FORGE_PAT", "forge-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Platforms: []config.PlatformConfig{{
			Type: "forgejo", Host: "code.example.com", TokenEnv: "FORGE_PAT",
		}},
		Repos: []config.Repo{
			{Owner: "org-a", Name: "one"},
			{Platform: "forgejo", PlatformHost: "code.example.com", Owner: "group", Name: "two"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "UNSET_ORG_A_PAT",
		}},
	}
	require.NoError(cfg.Validate())

	sources, err := collectProviderTokenSourcesDegraded(
		t.Context(), cfg, tokenauth.NewSourceSet(tokenauth.Options{}),
	)
	require.NoError(err,
		"a failing host must degrade instead of failing startup")
	assert.Contains(sources, providerHostKey("forgejo", "code.example.com"))
	assert.NotContains(sources, providerHostKey("github", "github.com"))
}

func TestBuildProviderControlPlaneOrDegradedKeepsHealthyProvidersWhenGitHubFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ORG_A_TOKEN", "org-a-token")
	t.Setenv("GITLAB_PAT", "gitlab-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Platforms: []config.PlatformConfig{{
			Type: "gitlab", Host: "gitlab.example.com", TokenEnv: "GITLAB_PAT",
		}},
		Repos: []config.Repo{
			{Owner: "org-a", Name: "one"},
			{Platform: "gitlab", PlatformHost: "gitlab.example.com", Owner: "group", Name: "two"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "ORG_A_TOKEN",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSourcesDegraded(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlaneOrDegraded(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{err: map[string]error{
			"ORG_A_TOKEN": errors.New("GitHub API unavailable: 503"),
		}},
	)
	require.NoError(err)
	require.Len(startup.registry.Providers(), 1,
		"the healthy GitLab host must keep syncing")
	assert.Empty(startup.githubClients)
	_, gitLabErr := startup.registry.RepositoryReader(
		platform.KindGitLab, "gitlab.example.com",
	)
	assert.NoError(gitLabErr)
}

func TestBuildProviderControlPlaneOrDegradedIsolatesFailingGitHubHost(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ORG_A_TOKEN", "org-a-token")
	t.Setenv("GHE_PAT", "ghe-token")
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Platforms: []config.PlatformConfig{{
			Type: "github", Host: "ghe.example.com", TokenEnv: "GHE_PAT",
		}},
		Repos: []config.Repo{
			{Owner: "org-a", Name: "one"},
			{PlatformHost: "ghe.example.com", Owner: "org-b", Name: "two"},
		},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: "github.com", Owner: "org-a", TokenEnv: "ORG_A_TOKEN",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{})
	sources, err := collectProviderTokenSourcesDegraded(t.Context(), cfg, set)
	require.NoError(err)

	startup, err := buildProviderControlPlaneOrDegraded(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		fakeGitHubIdentityResolver{
			byEnv: map[string]github.GitHubIdentity{
				"GHE_PAT": {Key: github.IdentityKey{
					Host: "ghe.example.com", Principal: "user:octo",
				}},
			},
			err: map[string]error{
				"ORG_A_TOKEN": errors.New("GitHub API unavailable: 503"),
			},
		},
	)
	require.NoError(err)
	assert.NotContains(startup.githubClients, "github.com",
		"the unavailable GitHub host must degrade to cached data")
	assert.Contains(startup.githubClients, "ghe.example.com",
		"the healthy GitHub host must keep syncing")
}

// A repository PAT override picks the credential that signs that repository's
// writes; it must not cost the owner its App installation. The installation
// carries its own rate-limit budget, so it still leads reads, and it remains
// the only credential that can enumerate a selected installation's repositories
// — a PAT cannot see selection-only grants.
func TestSelectedGitHubAppKeepsOwnerDiscoveryWhenRepoOverridesPAT(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("COVERED_REPO_PAT", "repo-pat")
	var installationAuth string
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet &&
			r.URL.Path == "/api/v3/installation/repositories" {
			installationAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"repositories":[{
				"id":101,"name":"covered","full_name":"acme/covered",
				"owner":{"login":"acme"},"private":true,"archived":false,"fork":false
			}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	originalTransport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(api.URL, "https://")

	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos: []config.Repo{{
			Platform: "github", PlatformHost: host,
			Owner: "acme", Name: "covered", TokenEnv: "COVERED_REPO_PAT",
		}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: host, AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 789, InstallationAccount: "acme",
			RepositorySelection: "selected", SelectedRepos: []string{"acme/covered"},
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "app-token", time.Now().Add(time.Hour), nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	database := dbtest.Open(t)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		tokenGitHubIdentityResolver{"repo-pat": {
			Key:   github.IdentityKey{Host: host, Principal: "user:5"},
			Login: "maintainer",
		}},
	)
	require.NoError(err)
	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)

	source := gitRoutes.SourceForRepo("github", host, "acme", "covered")
	require.NotNil(source)
	writeToken, err := source.Token(t.Context())
	require.NoError(err)
	assert.Equal("repo-pat", writeToken,
		"managed Git resolves under mutation auth, so it keeps the repo PAT")

	syncer := github.NewSyncerWithRegistry(
		startup.registry, database, nil, nil, time.Minute,
		startup.rateTrackers, startup.budgets,
	)
	t.Cleanup(syncer.Stop)
	syncer.SetGitHubRouters(startup.githubRouters)
	srv := server.NewWithConfig(
		database, syncer, nil, nil, cfg, t.TempDir()+"/config.toml",
		server.ServerOptions{
			TokenSources: set, HostCheckAllowLoopbackAnyPort: true,
		},
	)

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/repos/preview",
		strings.NewReader(fmt.Sprintf(
			`{"provider":"github","host":%q,"owner":"acme","pattern":"*"}`,
			host,
		)),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:8091"
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var response struct {
		Repos []struct {
			Name string `json:"name"`
		} `json:"repos"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&response))
	require.Len(response.Repos, 1)
	assert.Equal("covered", response.Repos[0].Name)
	assert.Equal("Bearer app-token", installationAuth,
		"owner discovery must still spend the installation's budget")
}

// A repository no configured route serves has no credential, and managed Git
// must say so rather than fall back to running git with no credential at all.
// Unauthenticated smart HTTP succeeds against any public repository, so a nil
// source would turn a missing route into a silent success that spends no
// identity's budget and reports no configuration problem.
func TestManagedGitFailsClosedForUnroutedGitHubRepository(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("ACME_PAT", "acme-secret")
	var requestCount atomic.Int32
	gitServer := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			requestCount.Add(1)
			w.WriteHeader(http.StatusOK)
		},
	))
	defer gitServer.Close()
	host := gitServer.Listener.Addr().String()
	cfg := &config.Config{
		SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
		Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
		Repos: []config.Repo{{
			Platform: "github", PlatformHost: host, Owner: "acme", Name: "widget",
		}},
		GitHubOwnerTokens: []config.GitHubOwnerTokenConfig{{
			Host: host, Owner: "acme", TokenEnv: "ACME_PAT",
		}},
	}
	require.NoError(cfg.Validate())
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)
	startup, err := buildProviderControlPlane(
		t.Context(), database, cfg, set, sources, defaultProviderFactories(),
		tokenGitHubIdentityResolver{"acme-secret": {
			Key: github.IdentityKey{Host: host, Principal: "user:1"},
		}},
	)
	require.NoError(err)
	gitRoutes := gitRoutesForProviderControlPlaneTest(t, cfg, set, &startup)

	manager := gitclone.New(t.TempDir(), gitRoutes)
	_, err = manager.RunGitForRepo(
		t.Context(), "github", host, "other", "thing", "",
		"ls-remote", gitServer.URL+"/other/thing.git",
	)

	require.Error(err, "an unrouted repository must not fall back to no credential")
	// gitclone flattens git failures into its own error type, so match the
	// message the route type produces rather than unwrapping to it.
	missing := &github.MissingRouteError{Host: host, Owner: "other", Name: "thing"}
	require.ErrorContains(err, missing.Error())
	assert.Zero(requestCount.Load(),
		"managed Git must not contact the remote without a credential route")
}

// An App-only selected installation plus a name pattern is a complete
// configuration: the App's exact routes serve its repositories and owner
// discovery expands the pattern. Startup must not demand an owner PAT the
// configuration deliberately omits.
//
// Startup must also not resolve the absent PAT once per repository. Every exact
// App route shares one PAT-less mutation chain ending in the gh CLI candidate,
// so an uncached verdict shells out per repository. The assertion is that the
// count does not grow with the installation, which is the property that matters
// and does not depend on how many fixed-cost lookups startup makes elsewhere.
func TestAppOnlySelectedGlobStartsWithoutPATAndResolvesMissingPATOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	startupGhCLICalls := func(t *testing.T, selected []string) int {
		t.Helper()
		database := dbtest.Open(t)
		var ghCLICalls atomic.Int32
		api := httptest.NewTLSServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet &&
					r.URL.Path == "/api/v3/installation/repositories" {
					_, _ = io.WriteString(w, `{"repositories":[]}`)
					return
				}
				http.NotFound(w, r)
			},
		))
		t.Cleanup(api.Close)
		originalTransport := http.DefaultTransport
		http.DefaultTransport = api.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = originalTransport })
		host := strings.TrimPrefix(api.URL, "https://")

		cfg := &config.Config{
			SyncInterval: "5m", Host: "127.0.0.1", Port: 8091, BasePath: "/",
			Activity: config.Activity{ViewMode: "flat", TimeRange: "7d"},
			Repos: []config.Repo{{
				Platform: "github", PlatformHost: host, Owner: "acme", Name: "*",
			}},
			GitHubApps: []config.GitHubAppConfig{{
				Host: host, AppID: 7, PrivateKeyPath: "/keys/app.pem",
				InstallationID: 789, InstallationAccount: "acme",
				RepositorySelection: "selected", SelectedRepos: selected,
			}},
		}
		require.NoError(cfg.Validate())
		set := tokenauth.NewSourceSet(tokenauth.Options{
			GitHubCLI: func(context.Context, string) (string, error) {
				ghCLICalls.Add(1)
				return "", tokenauth.ErrMissingToken
			},
			GitHubApp: func(
				context.Context, tokenauth.Candidate,
			) (string, time.Time, error) {
				return "app-token", time.Now().Add(time.Hour), nil
			},
		})
		sources, err := collectProviderTokenSources(t.Context(), cfg, set)
		require.NoError(err)

		// This resolver resolves the mutation chain for real, so an uncached
		// missing-PAT verdict reaches the gh CLI candidate once per route.
		startup, err := buildProviderControlPlane(
			t.Context(), database, cfg, set, sources, defaultProviderFactories(),
			tokenGitHubIdentityResolver{},
		)
		require.NoError(err,
			"an App-only selected installation must not require an owner PAT")
		require.NotNil(startup.githubRouters[host])
		_, err = startup.githubRouters[host].RouteForRepo("acme", selected[0][len("acme/"):])
		require.NoError(err,
			"the App's exact route must serve its selected repository")
		return int(ghCLICalls.Load())
	}

	small := startupGhCLICalls(t, []string{"acme/one", "acme/two"})
	large := startupGhCLICalls(t, []string{
		"acme/one", "acme/two", "acme/three", "acme/four", "acme/five",
	})

	assert.Equal(small, large,
		"resolving the absent PAT must not cost one lookup per repository")
}
