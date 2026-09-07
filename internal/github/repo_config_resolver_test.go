package github

import (
	"context"
	"errors"
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/platform"
)

func TestResolveConfiguredRepos_ExpandsGlobIncludingArchived(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("widgets"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("widgets-api"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("widgets-legacy"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(true),
				},
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{{Owner: "acme", Name: "widgets-*"}},
	)

	require.Len(t, result.Configured, 1)
	assert.Equal(2, result.Configured[0].MatchedRepoCount)
	assert.Equal([]RepoRef{
		{
			Platform:     platform.KindGitHub,
			Owner:        "acme",
			Name:         "widgets-api",
			PlatformHost: "github.com",
			RepoPath:     "acme/widgets-api",
		},
		{
			Platform:     platform.KindGitHub,
			Owner:        "acme",
			Name:         "widgets-legacy",
			PlatformHost: "github.com",
			RepoPath:     "acme/widgets-legacy",
			Archived:     true,
		},
	}, result.Expanded)
}

func TestResolveConfiguredRepos_AcceptsArchivedRepoAsArchiveOnly(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(true),
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{{Owner: "acme", Name: "widgets-legacy"}},
	)

	assert.Empty(result.Warnings)
	require.Len(t, result.Configured, 1)
	assert.Equal(1, result.Configured[0].MatchedRepoCount)
	assert.Equal([]RepoRef{{
		Platform:           platform.KindGitHub,
		Owner:              "acme",
		Name:               "widgets-legacy",
		PlatformHost:       "github.com",
		RepoPath:           "acme/widgets-legacy",
		Archived:           true,
		ConfiguredRepoPath: "acme/widgets-legacy",
	}}, result.Expanded)
}

func TestExpandedRepoSetPrefersResolvedOverFallbackDuplicates(t *testing.T) {
	assert := assert.New(t)
	fallback := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "frozen",
		PlatformHost: "github.com", RepoPath: "acme/frozen",
	}
	resolved := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "frozen",
		PlatformHost: "github.com", RepoPath: "acme/frozen",
		PlatformExternalID: "repo-acme-frozen", Archived: true,
	}

	set := NewExpandedRepoSet()
	set.Add(fallback, false)
	set.Add(resolved, true)
	assert.Equal([]RepoRef{resolved}, set.Refs(),
		"a provider-resolved duplicate must replace fallback metadata")

	// The reverse order keeps the resolved ref: fallback metadata never
	// overwrites a successful resolution.
	set = NewExpandedRepoSet()
	set.Add(resolved, true)
	set.Add(fallback, false)
	assert.Equal([]RepoRef{resolved}, set.Refs())

	// Same-class duplicates keep the first entry.
	set = NewExpandedRepoSet()
	set.Add(resolved, true)
	other := resolved
	other.Archived = false
	set.Add(other, true)
	assert.Equal([]RepoRef{resolved}, set.Refs())
}

func TestExpandedRepoSetReconcilesRenamedRouteByProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	// A fallback ref keeps the old route of a renamed repo; the resolved
	// duplicate arrives under the new route with the same stable provider
	// id. Route-keyed dedup alone would track both and sync them twice.
	fallback := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "old-name",
		PlatformHost: "github.com", RepoPath: "acme/old-name",
		PlatformExternalID: "repo-x",
	}
	resolved := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "new-name",
		PlatformHost: "github.com", RepoPath: "acme/new-name",
		PlatformExternalID: "repo-x", Archived: true,
	}

	set := NewExpandedRepoSet()
	set.Add(fallback, false)
	set.Add(resolved, true)
	assert.Equal([]RepoRef{resolved}, set.Refs())

	// The resolved route survives a later fallback under the old route.
	set = NewExpandedRepoSet()
	set.Add(resolved, true)
	set.Add(fallback, false)
	assert.Equal([]RepoRef{resolved}, set.Refs())
}

func TestExpandedRepoSetMergesExactProvenanceAcrossDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The exact entry's fallback preserved a renamed tracked ref with its
	// config-entry provenance; the overlapping glob resolves the same
	// provider id without any (glob refs carry none). Whichever ref wins
	// the slot, losing the exact provenance would leave the next failed
	// reload unable to correlate the entry and synthesize a duplicate.
	exactFallback := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-acme-tools",
		ConfiguredRepoPath: "acme/tools", Archived: true,
	}
	resolvedGlob := RepoRef{
		Platform: platform.KindGitHub, Owner: "acme", Name: "tools-new",
		PlatformHost: "github.com", RepoPath: "acme/tools-new",
		PlatformExternalID: "repo-acme-tools",
		WebURL:             "https://github.com/acme/tools-new",
		Archived:           true,
	}

	set := NewExpandedRepoSet()
	set.Add(exactFallback, false)
	set.Add(resolvedGlob, true)
	refs := set.Refs()
	require.Len(refs, 1)
	assert.Equal("https://github.com/acme/tools-new", refs[0].WebURL)
	assert.Equal("acme/tools", refs[0].ConfiguredRepoPath)

	set = NewExpandedRepoSet()
	set.Add(resolvedGlob, true)
	set.Add(exactFallback, false)
	refs = set.Refs()
	require.Len(refs, 1)
	assert.Equal("https://github.com/acme/tools-new", refs[0].WebURL)
	assert.Equal("acme/tools", refs[0].ConfiguredRepoPath)
}

func TestResolveConfiguredRepos_DeduplicatesExactAndGlobMatches(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new(owner)},
				Archived: new(false),
			}, nil
		},
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("widgets"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
				{
					Name:     new("widgets-api"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{
			{Owner: "acme", Name: "widgets"},
			{Owner: "acme", Name: "widgets*"},
		},
	)

	assert.Len(result.Expanded, 2)
	assert.ElementsMatch([]RepoRef{
		{Platform: platform.KindGitHub, Owner: "acme", Name: "widgets", PlatformHost: "github.com", RepoPath: "acme/widgets", ConfiguredRepoPath: "acme/widgets"},
		{Platform: platform.KindGitHub, Owner: "acme", Name: "widgets-api", PlatformHost: "github.com", RepoPath: "acme/widgets-api"},
	}, result.Expanded)
}

func TestResolveConfiguredRepos_DeduplicatesOwnerCase(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		getRepositoryFn: func(
			_ context.Context, owner, repo string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new(repo),
				Owner:    &gh.User{Login: new("acme")},
				Archived: new(false),
			}, nil
		},
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("widgets"),
					Owner:    &gh.User{Login: new("acme")},
					Archived: new(false),
				},
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{
			{Owner: "Acme", Name: "widgets"},
			{Owner: "acme", Name: "widgets*"},
		},
	)

	assert.Equal([]RepoRef{{
		Platform:           platform.KindGitHub,
		Owner:              "acme",
		Name:               "widgets",
		PlatformHost:       "github.com",
		RepoPath:           "acme/widgets",
		ConfiguredRepoPath: "Acme/widgets",
	}}, result.Expanded)
}

func TestResolveConfiguredReposCasefoldsResolvedRepoRefs(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		getRepositoryFn: func(
			_ context.Context, _, _ string,
		) (*gh.Repository, error) {
			return &gh.Repository{
				Name:     new("Foo"),
				Owner:    &gh.User{Login: new("Org")},
				Archived: new(false),
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{{Owner: "org", Name: "foo"}},
	)

	assert.Equal([]RepoRef{{
		Platform:           platform.KindGitHub,
		Owner:              "org",
		Name:               "foo",
		PlatformHost:       "github.com",
		RepoPath:           "org/foo",
		ConfiguredRepoPath: "org/foo",
	}}, result.Expanded)
}

func TestResolveConfiguredRepos_ReportsZeroCountOnStartupWarning(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		listReposByOwnerFn: func(
			_ context.Context, owner string,
		) ([]*gh.Repository, error) {
			return nil, errors.New("boom")
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{{Owner: "acme", Name: "widgets-*"}},
	)

	require.Len(t, result.Configured, 1)
	assert.True(result.Configured[0].IsGlob)
	assert.Equal(0, result.Configured[0].MatchedRepoCount)
	assert.Empty(result.Expanded)
	assert.Len(result.Warnings, 1)
}

func TestResolveConfiguredRepos_MatchesRepoNamesCaseInsensitively(t *testing.T) {
	assert := assert.New(t)
	client := &mockClient{
		listReposByOwnerFn: func(_ context.Context, owner string) ([]*gh.Repository, error) {
			return []*gh.Repository{
				{
					Name:     new("Widget-API"),
					Owner:    &gh.User{Login: new(owner)},
					Archived: new(false),
				},
			}, nil
		},
	}

	result := ResolveConfiguredRepos(
		t.Context(),
		map[string]Client{"github.com": client},
		[]config.Repo{{Owner: "acme", Name: "widget-*"}},
	)

	require.Len(t, result.Configured, 1)
	assert.Equal(1, result.Configured[0].MatchedRepoCount)
	assert.Equal([]RepoRef{{
		Platform:     platform.KindGitHub,
		Owner:        "acme",
		Name:         "widget-api",
		PlatformHost: "github.com",
		RepoPath:     "acme/widget-api",
	}}, result.Expanded)
}

func TestResolveConfiguredReposReportsMissingProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result := resolveConfiguredRepos(
		t.Context(),
		mustRegistry(t),
		[]config.Repo{{
			Platform:     "gitlab",
			PlatformHost: "gitlab.com",
			Owner:        "acme",
			Name:         "widget",
		}},
	)

	require.Len(result.Warnings, 1)
	var platformErr *platform.Error
	require.ErrorAs(result.Warnings[0], &platformErr)
	require.ErrorIs(result.Warnings[0], platform.ErrProviderNotConfigured)
	assert.Equal(platform.KindGitLab, platformErr.Provider)
	assert.Equal("gitlab.com", platformErr.PlatformHost)
}

func TestResolveConfiguredReposReportsMissingRepositoryReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	result := resolveConfiguredRepos(
		t.Context(),
		mustRegistry(t, resolverTestProvider{
			kind: platform.KindGitLab,
			host: "gitlab.com",
		}),
		[]config.Repo{{
			Platform:     "gitlab",
			PlatformHost: "gitlab.com",
			Owner:        "acme",
			Name:         "widget",
		}},
	)

	require.Len(result.Warnings, 1)
	var platformErr *platform.Error
	require.ErrorAs(result.Warnings[0], &platformErr)
	require.ErrorIs(result.Warnings[0], platform.ErrUnsupportedCapability)
	assert.Equal("read_repositories", platformErr.Capability)
}

func TestResolveConfiguredReposKeepsDuplicateOwnerNameOnDifferentPlatforms(t *testing.T) {
	result := resolveConfiguredRepos(
		t.Context(),
		mustRegistry(t,
			resolverRepositoryReader{
				kind: platform.KindGitHub,
				host: "code.example.com",
			},
			resolverRepositoryReader{
				kind: platform.KindGitLab,
				host: "code.example.com",
			},
		),
		[]config.Repo{
			{
				Platform:     "github",
				PlatformHost: "code.example.com",
				Owner:        "acme",
				Name:         "widget",
			},
			{
				Platform:     "gitlab",
				PlatformHost: "code.example.com",
				Owner:        "acme",
				Name:         "widget",
			},
		},
	)

	require.Empty(t, result.Warnings)
	assert.ElementsMatch(t, []RepoRef{
		{
			Platform:           platform.KindGitHub,
			PlatformHost:       "code.example.com",
			Owner:              "acme",
			Name:               "widget",
			RepoPath:           "acme/widget",
			ConfiguredRepoPath: "acme/widget",
		},
		{
			Platform:           platform.KindGitLab,
			PlatformHost:       "code.example.com",
			Owner:              "acme",
			Name:               "widget",
			RepoPath:           "acme/widget",
			ConfiguredRepoPath: "acme/widget",
		},
	}, result.Expanded)
}

func TestFallbackConfiguredRepoRefsSynthesizesGitHubProvider(t *testing.T) {
	assert := assert.New(t)

	got := FallbackConfiguredRepoRefs(nil, config.Repo{
		Platform:     "github",
		PlatformHost: "code.example.com",
		Owner:        "Acme",
		Name:         "Widget",
	})

	assert.Equal([]RepoRef{{
		Platform:           platform.KindGitHub,
		PlatformHost:       "code.example.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "Acme/Widget",
		ConfiguredRepoPath: "Acme/Widget",
	}}, got)
}

func TestFallbackConfiguredRepoRefsPreservesProviderIdentity(t *testing.T) {
	assert := assert.New(t)
	previous := []RepoRef{
		{
			Platform:     platform.KindGitHub,
			PlatformHost: "code.example.com",
			Owner:        "acme",
			Name:         "widget",
		},
		{
			Platform:     platform.KindGitLab,
			PlatformHost: "code.example.com",
			Owner:        "acme",
			Name:         "widget",
		},
	}

	got := FallbackConfiguredRepoRefs(previous, config.Repo{
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	})

	assert.Equal([]RepoRef{{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget",
	}}, got)
}

func TestRepoRefFromRepositoryStampsConfiguredRepoPath(t *testing.T) {
	assert := assert.New(t)

	ref := repoRefFromRepository(
		config.Repo{Owner: "acme", Name: "tools"},
		platform.KindGitHub, "github.com",
		platform.Repository{
			Ref: platform.RepoRef{
				Owner: "acme", Name: "tools-new",
				RepoPath: "acme/tools-new",
			},
			PlatformExternalID: "repo-acme-tools",
		},
	)

	assert.Equal("acme/tools", ref.ConfiguredRepoPath)
}

func TestFallbackConfiguredRepoRefsMatchesRenamedRouteByConfiguredPath(t *testing.T) {
	assert := assert.New(t)
	renamed := RepoRef{
		Platform:           platform.KindGitHub,
		PlatformHost:       "github.com",
		Owner:              "acme",
		Name:               "tools-new",
		RepoPath:           "acme/tools-new",
		PlatformExternalID: "repo-acme-tools",
		ConfiguredRepoPath: "acme/tools",
		Archived:           true,
	}

	got := FallbackConfiguredRepoRefs([]RepoRef{renamed}, config.Repo{
		Owner: "acme",
		Name:  "tools",
	})

	assert.Equal([]RepoRef{renamed}, got)
}

func TestFallbackConfiguredRepoRefsSynthesizesNonGitHubProvider(t *testing.T) {
	assert := assert.New(t)

	got := FallbackConfiguredRepoRefs(nil, config.Repo{
		Platform: "gitlab",
		Owner:    "Acme/SubGroup",
		Name:     "Widget",
	})

	assert.Equal([]RepoRef{{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "Acme/SubGroup",
		Name:               "Widget",
		RepoPath:           "Acme/SubGroup/Widget",
		ConfiguredRepoPath: "Acme/SubGroup/Widget",
	}}, got)
}

func TestFallbackConfiguredRepoRefsGlobFiltersByProvider(t *testing.T) {
	assert := assert.New(t)
	previous := []RepoRef{
		{
			Platform:     platform.KindGitHub,
			PlatformHost: "code.example.com",
			Owner:        "acme",
			Name:         "widget-api",
		},
		{
			Platform:     platform.KindGitLab,
			PlatformHost: "code.example.com",
			Owner:        "acme",
			Name:         "widget-api",
		},
	}

	got := FallbackConfiguredRepoRefs(previous, config.Repo{
		Platform:     "gitlab",
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget-*",
	})

	assert.Equal([]RepoRef{{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "acme",
		Name:         "widget-api",
	}}, got)
}

func TestResolveConfiguredReposWithRegistryUsesNonGitHubProvider(t *testing.T) {
	result := ResolveConfiguredReposWithRegistry(
		t.Context(),
		mustRegistry(t, resolverRepositoryReader{
			kind: platform.KindGitLab,
			host: "gitlab.com",
		}),
		[]config.Repo{{
			Platform:     "gitlab",
			PlatformHost: "gitlab.com",
			Owner:        "acme/subgroup",
			Name:         "widget",
		}},
	)

	require.Empty(t, result.Warnings)
	assert.Equal(t, []RepoRef{{
		Platform:           platform.KindGitLab,
		PlatformHost:       "gitlab.com",
		Owner:              "acme/subgroup",
		Name:               "widget",
		RepoPath:           "acme/subgroup/widget",
		ConfiguredRepoPath: "acme/subgroup/widget",
	}}, result.Expanded)
}

func mustRegistry(t *testing.T, providers ...platform.Provider) *platform.Registry {
	t.Helper()
	registry, err := platform.NewRegistry(providers...)
	require.NoError(t, err)
	return registry
}

type resolverTestProvider struct {
	kind platform.Kind
	host string
}

func (p resolverTestProvider) Platform() platform.Kind {
	return p.kind
}

func (p resolverTestProvider) Host() string {
	return p.host
}

func (p resolverTestProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{}
}

type resolverRepositoryReader struct {
	resolverTestProvider
}

func (r resolverRepositoryReader) GetRepository(
	_ context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	return platform.Repository{
		Ref: platform.RepoRef{
			Platform: ref.Platform,
			Host:     ref.Host,
			Owner:    ref.Owner,
			Name:     ref.Name,
		},
	}, nil
}

func (r resolverRepositoryReader) ListRepositories(
	context.Context,
	string,
	platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	return nil, nil
}
