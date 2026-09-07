package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
)

// TestBuildProviderControlPlaneScopesWriteTrackersToAppChains pins the
// split-credential decision to the host's effective token chain, not
// [[github_apps]] config: a host whose chain carries an active
// github_app candidate gets dedicated write trackers, while a host
// resolving through plain env credentials (for example because every
// repo carries a terminal token override) must not — an empty write
// tracker would shadow the shared trackers that already observed the
// exhausted credential.
func TestBuildProviderControlPlaneScopesWriteTrackersToAppChains(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	t.Setenv("SPLIT_TEST_PAT", "pat-token")

	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_split", time.Now().Add(time.Hour), nil
		},
	})
	appChain := set.Upsert(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.com"},
		Candidates: []tokenauth.Candidate{
			{
				Kind:           tokenauth.SourceKindGitHubApp,
				Host:           "github.com",
				FilePath:       "/keys/app.pem",
				AppID:          7,
				InstallationID: 11,
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "SPLIT_TEST_PAT"},
		},
	})
	envChain := set.Upsert(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "ghe.example.com"},
		Candidates: []tokenauth.Candidate{
			{Kind: tokenauth.SourceKindEnv, EnvName: "SPLIT_TEST_PAT"},
		},
	})

	startup, err := buildProviderControlPlane(
		t.Context(), database,
		&config.Config{SyncBudgetPerHour: 200},
		set,
		map[string]tokenauth.Source{
			providerHostKey("github", "github.com"):      appChain,
			providerHostKey("github", "ghe.example.com"): envChain,
		},
		defaultProviderFactories(), nil,
	)
	require.NoError(err)
	require.NotNil(startup.quotaRegistry)

	appKey := github.RateBucketKey(string(platform.KindGitHub), "github.com", "host")
	envKey := github.RateBucketKey(string(platform.KindGitHub), "ghe.example.com", "host")
	assert.Contains(startup.writeRateTrackers, appKey)
	assert.Contains(startup.writeGQLRateTrackers, appKey)
	assert.NotContains(startup.writeRateTrackers, envKey,
		"shared-credential hosts must keep gating on the sync trackers")
	assert.NotContains(startup.writeGQLRateTrackers, envKey)
	assert.Same(startup.quotaRegistry, startup.fetchers["github.com"].QuotaRegistry())
	assert.Same(startup.quotaRegistry, startup.fetchers["ghe.example.com"].QuotaRegistry())
}
