package main

import (
	"context"
	appfiles "go.kenn.io/forge/internal/githubapp"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/githubapp"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/githubapp/githubapptest"
	"go.kenn.io/forge/internal/tokenauth"
)

// TestCollectProviderTokensMintsGitHubAppToken pins the startup wiring
// from TOML github_apps config through the production token collector
// to a minted installation token: the resolved github.com source must
// authenticate with an app token even when a PAT env var is set,
// because taking sync traffic off the PAT is the feature.
func TestCollectProviderTokensMintsGitHubAppToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fake := githubapptest.NewFake()
	t.Cleanup(fake.Close)

	// Register an app + installation on the fake GitHub the way the
	// kenn-forge-github-app CLI would have.
	manifest, err := appfiles.NewManifest(
		"kenn-forge-startup", "", "http://127.0.0.1:1/callback",
	)
	require.NoError(err)
	manifestJSON, err := manifest.JSON()
	require.NoError(err)
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.PostForm(
		fake.URL()+"/settings/apps/new", url.Values{"manifest": {manifestJSON}},
	)
	require.NoError(err)
	defer resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(err)
	apiClient := githubapp.NewClientWithBase(fake.APIBase())
	creds, err := apiClient.ConvertManifest(t.Context(), loc.Query().Get("code"))
	require.NoError(err)
	installID, err := fake.Install(creds.ID, "kenn-io")
	require.NoError(err)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app.pem")
	require.NoError(os.WriteFile(keyPath, []byte(creds.PEM), 0o600))
	configPath := filepath.Join(dir, "config.toml")
	configTOML := `
host = "127.0.0.1"
port = 8091

[[repos]]
owner = "kenn-io"
name = "kenn-forge"

[[github_apps]]
host = "github.com"
app_id = ` + strconv.FormatInt(creds.ID, 10) + `
slug = "kenn-forge-startup"
private_key_path = "app.pem"
installation_id = ` + strconv.FormatInt(installID, 10) + `
installation_account = "kenn-io"
repository_selection = "all"
`
	require.NoError(os.WriteFile(configPath, []byte(configTOML), 0o644))
	cfg, err := config.Load(configPath)
	require.NoError(err)

	// The PAT must lose to the app token.
	t.Setenv("KENN_FORGE_GITHUB_TOKEN", "pat-should-not-be-used")

	// Same Options shape main() wires, with the API base pointed at
	// the fake instead of api.github.com.
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubCLI: config.GitHubCLITokenForHost,
		GitHubApp: func(
			ctx context.Context, candidate tokenauth.Candidate,
		) (string, time.Time, error) {
			key, err := appfiles.LoadPrivateKey(candidate.FilePath)
			if err != nil {
				return "", time.Time{}, err
			}
			jwt, err := githubapp.SignAppJWT(candidate.AppID, key, time.Now())
			if err != nil {
				return "", time.Time{}, err
			}
			tok, err := apiClient.CreateInstallationToken(
				ctx, jwt, candidate.InstallationID,
			)
			if err != nil {
				return "", time.Time{}, err
			}
			return tok.Token, tok.ExpiresAt, nil
		},
	})
	sources, err := collectProviderTokenSources(t.Context(), cfg, set)
	require.NoError(err)

	key := providerHostKey("github", "github.com")
	require.Contains(sources, key)
	got, err := sources[key].Token(tokenauth.WithGitHubOwner(t.Context(), "kenn-io"))
	require.NoError(err)
	assert.True(strings.HasPrefix(got, "ghs_"),
		"expected a minted installation token, got %q", got)

	// The minted token must be live on GitHub's side, not just shaped
	// like one: the fake only honors tokens it issued.
	rate, err := apiClient.CoreRateLimit(t.Context(), got)
	require.NoError(err)
	assert.Equal(5000, rate.Limit)
}

func TestCollectProviderTokensValidatesGitHubAppPerRepoOwner(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(`
[[repos]]
owner = "kenn-io"
name = "kenn-forge"

[[repos]]
owner = "mariusvniekerk"
name = "skills"

[[github_apps]]
host = "github.com"
app_id = 4321
private_key_path = "app.pem"
installation_id = 99
installation_account = "kenn-io"
repository_selection = "all"
`), 0o600))
	cfg, err := config.Load(configPath)
	require.NoError(err)

	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(_ context.Context, c tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_kenn", time.Now().Add(time.Hour), nil
		},
	})
	_, err = collectProviderTokenSources(t.Context(), cfg, set)
	require.Error(err)
	assert.ErrorContains(t, err, "owner mariusvniekerk")
}
