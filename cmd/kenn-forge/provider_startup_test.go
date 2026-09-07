package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/gitclone"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

func TestBuildServeControlPlanesNodeNeverConstructsProviderPlane(t *testing.T) {
	assert := assert.New(t)
	var factoryCalls atomic.Int32
	var appMints atomic.Int32
	cfg := &config.Config{
		Fleet: config.Fleet{
			Enabled: true, Role: config.FleetRoleSpoke,
			Hub: &config.FleetHub{
				NodeID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BaseURL: "https://hub.example",
			},
		},
		Repos: []config.Repo{{Owner: "acme", Name: "widget"}},
		GitHubApps: []config.GitHubAppConfig{{
			Host: "github.com", AppID: 7, PrivateKeyPath: "/keys/app.pem",
			InstallationID: 11, InstallationAccount: "acme",
			RepositorySelection: "all",
		}},
	}
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			appMints.Add(1)
			return "provider-token", time.Now().Add(time.Hour), nil
		},
	})
	planes, err := buildServeControlPlanes(
		t.Context(), nil, cfg, set,
		map[string]providerFactory{
			"github": func(context.Context, providerFactoryInput) (providerFactoryOutput, error) {
				factoryCalls.Add(1)
				return providerFactoryOutput{}, nil
			},
		},
		fakeGitHubIdentityResolver{}, false,
	)
	require.NoError(t, err)
	assert.Nil(planes.Provider)
	assert.Zero(factoryCalls.Load())
	assert.Zero(appMints.Load())
	assert.NotNil(planes.Git.SourceForRepo(
		"github", "github.com", "acme", "widget",
	))
}

func TestDisableSyncRetainsHubProviderPlaneButNeverCreatesNodePlane(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	hub, err := buildServeControlPlanes(
		t.Context(), dbtest.Open(t), &config.Config{}, set,
		defaultProviderFactories(), nil, true,
	)
	require.NoError(err)
	require.NotNil(hub.Provider)
	assert.NotNil(hub.Provider.registry)
	provider, err := hub.Provider.registry.Provider(
		platform.KindGitHub, platform.DefaultGitHubHost,
	)
	require.NoError(err)
	assert.NotNil(provider)

	spoke, err := buildServeControlPlanes(
		t.Context(), nil, &config.Config{Fleet: config.Fleet{
			Enabled: true, Role: config.FleetRoleSpoke,
			Hub: &config.FleetHub{
				NodeID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BaseURL: "https://hub.example",
			},
		}}, set, defaultProviderFactories(), nil, true,
	)
	require.NoError(err)
	assert.Nil(spoke.Provider)
}

func TestGitStartupAllowsAnonymousReadsButRequiredRoutesFailClosed(t *testing.T) {
	require := require.New(t)
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: func(context.Context, string) (string, error) {
			return "", tokenauth.ErrMissingToken
		},
	})
	optional, err := buildGitStartup(&config.Config{}, set)
	require.NoError(err)

	publicSource := optional.SourceForRepo(
		string(platform.KindGitHub), "github.com", "acme", "public",
	)
	require.NotNil(publicSource)
	token, err := publicSource.Token(t.Context())
	require.NoError(err)
	assert.Empty(t, token, "an optional missing credential must preserve anonymous Git")
	manager := gitclone.New(t.TempDir(), &optional)
	require.ErrorIs(manager.RequireCredentialRoute(
		t.Context(), string(platform.KindGitHub), "github.com", "acme", "public",
	), gitclone.ErrCredentialUnavailable)

	requiredRoutes, err := buildGitStartup(&config.Config{
		Repos: []config.Repo{{Owner: "acme", Name: "private"}},
	}, set)
	require.NoError(err)
	requiredSource := requiredRoutes.SourceForRepo(
		string(platform.KindGitHub), "github.com", "acme", "private",
	)
	require.NotNil(requiredSource)
	_, err = requiredSource.Token(t.Context())
	require.ErrorIs(err, tokenauth.ErrMissingToken)
}

func TestDefaultProviderFactoryPassesGitLabSharedSyncBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("factory-token", r.Header.Get("Private-Token"))
		assert.Equal("/api/v4/projects/42", r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"path":"project","path_with_namespace":"group/project","name":"Project"}`))
	}))
	defer server.Close()

	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	host := strings.TrimPrefix(server.URL, "https://")
	budget := github.NewSyncBudget(100)
	factory := defaultProviderFactories()[string(platform.KindGitLab)]
	built, err := factory(t.Context(), providerFactoryInput{
		host: host,
		tokenSource: mainTestTokenSource(
			t, string(platform.KindGitLab), host, "GITLAB_FACTORY_TOKEN", "factory-token",
		),
		budget: budget,
	})
	require.NoError(err)
	reader, ok := built.provider.(platform.RepositoryReader)
	require.True(ok)

	_, err = reader.GetRepository(github.WithSyncBudget(t.Context()), platform.RepoRef{
		Platform: platform.KindGitLab, Host: host,
		Owner: "group", Name: "project", RepoPath: "group/project", PlatformID: 42,
	})
	require.NoError(err)
	assert.Equal(1, budget.Spent())
}

func TestDefaultProviderFactoryUsesGiteaExplicitBaseURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("token factory-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"version":"1.26.0"}`))
		case "/api/v1/repos/owner/repo":
			_, _ = w.Write([]byte(`{
				"id":42,"name":"repo","full_name":"owner/repo",
				"clone_url":"http://gitea.test/owner/repo.git",
				"owner":{"login":"owner"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	factory := defaultProviderFactories()[string(platform.KindGitea)]
	built, err := factory(t.Context(), providerFactoryInput{
		host:          "gitea.test",
		baseURL:       server.URL,
		allowInsecure: true,
		tokenSource: mainTestTokenSource(
			t, string(platform.KindGitea), "gitea.test", "GITEA_FACTORY_TOKEN", "factory-token",
		),
	})
	require.NoError(err)
	reader, ok := built.provider.(platform.RepositoryReader)
	require.True(ok)

	repo, err := reader.GetRepository(t.Context(), platform.RepoRef{
		Platform: platform.KindGitea, Host: "gitea.test", Owner: "owner", Name: "repo",
	})
	require.NoError(err)
	assert.Equal("http://gitea.test/owner/repo.git", repo.CloneURL)
}
