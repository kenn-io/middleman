package githubapp

import (
	"context"
	"fmt"
	"go.kenn.io/forge/githubapp"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/githubapp/githubapptest"
)

// submitManifest plays the browser role: POST a manifest form to the
// fake's web surface and return the conversion code from the redirect.
func submitManifest(t *testing.T, fake *githubapptest.Fake, manifest githubapp.Manifest) string {
	t.Helper()
	manifestJSON, err := manifest.JSON()
	require.NoError(t, err)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.PostForm(
		fake.URL()+"/settings/apps/new?state=test-state",
		url.Values{"manifest": {manifestJSON}},
	)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "test-state", loc.Query().Get("state"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)
	return code
}

func TestConvertManifest(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	fake := githubapptest.NewFake()
	t.Cleanup(fake.Close)
	manifest, err := NewManifest("kenn-forge-conv", "", "http://127.0.0.1:1/callback")
	require.NoError(err)
	code := submitManifest(t, fake, manifest)

	client := githubapp.NewClientWithBase(fake.APIBase())
	creds, err := client.ConvertManifest(context.Background(), code)
	require.NoError(err)

	assert := assert.New(t)
	assert.Equal("kenn-forge-conv", creds.Slug)
	assert.Positive(creds.ID)
	assert.Contains(creds.PEM, "RSA PRIVATE KEY")
	assert.NotEmpty(creds.ClientSecret)
	_, parseErr := githubapp.ParsePrivateKey([]byte(creds.PEM))
	require.NoError(parseErr)

	// Conversion codes are single use; replay must fail loudly so the
	// CLI reports a stale callback instead of silently re-creating.
	_, err = client.ConvertManifest(context.Background(), code)
	assert.True(githubapp.IsStatus(err, http.StatusNotFound), "got %v", err)
}

func TestMintInstallationToken(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	fake := githubapptest.NewFake()
	t.Cleanup(fake.Close)
	manifest, err := NewManifest("kenn-forge-mint", "", "http://127.0.0.1:1/callback")
	require.NoError(err)
	code := submitManifest(t, fake, manifest)
	client := githubapp.NewClientWithBase(fake.APIBase())
	creds, err := client.ConvertManifest(context.Background(), code)
	require.NoError(err)
	installID, err := fake.Install(creds.ID, "kenn-io")
	require.NoError(err)

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(os.WriteFile(keyPath, []byte(creds.PEM), 0o600))

	token, expires, err := mintInstallationToken(
		context.Background(), fake.APIBase(), creds.ID, keyPath, installID,
	)
	require.NoError(err)
	assert := assert.New(t)
	assert.True(strings.HasPrefix(token, "ghs_"), "token %q", token)
	assert.Greater(time.Until(expires), 50*time.Minute)

	// The minted token must be usable as a plain bearer credential.
	rate, err := client.CoreRateLimit(context.Background(), token)
	require.NoError(err)
	assert.Equal(5000, rate.Limit)
}

func TestMintInstallationTokenRejectsWrongKey(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	fake := githubapptest.NewFake()
	t.Cleanup(fake.Close)
	manifest, err := NewManifest("kenn-forge-badkey", "", "http://127.0.0.1:1/callback")
	require.NoError(err)
	client := githubapp.NewClientWithBase(fake.APIBase())
	creds, err := client.ConvertManifest(
		context.Background(), submitManifest(t, fake, manifest),
	)
	require.NoError(err)
	installID, err := fake.Install(creds.ID, "kenn-io")
	require.NoError(err)

	// A key that does not match the app must be rejected by signature
	// verification, not just shape checks.
	otherKey := generateTestKey(t)
	wrongJWT, err := githubapp.SignAppJWT(creds.ID, otherKey, time.Now())
	require.NoError(err)
	_, err = client.CreateInstallationToken(context.Background(), wrongJWT, installID)
	assert.True(t, githubapp.IsStatus(err, http.StatusUnauthorized), "got %v", err)
}

func TestAPIBaseForHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host string
		want string
	}{
		{host: "", want: "https://api.github.com"},
		{host: "github.com", want: "https://api.github.com"},
		{host: "github.example.com", want: "https://github.example.com/api/v3"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, githubapp.APIBaseForHost(tt.host), "host %q", tt.host)
	}
}

func TestStatusErrorRetryDeadline(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	rateResetHeader := make(http.Header)
	rateResetHeader.Set("X-RateLimit-Reset", fmt.Sprint(now.Add(10*time.Minute).Unix()))
	rateResetHeader.Set("X-RateLimit-Remaining", "0")
	unrelatedResetHeader := make(http.Header)
	unrelatedResetHeader.Set("X-RateLimit-Reset", fmt.Sprint(now.Add(10*time.Minute).Unix()))
	unrelatedResetHeader.Set("X-RateLimit-Remaining", "4999")
	for _, tt := range []struct {
		name   string
		header http.Header
		want   time.Time
	}{
		{
			name:   "retry after seconds",
			header: http.Header{"Retry-After": {"90"}},
			want:   now.Add(90 * time.Second),
		},
		{
			name:   "rate limit reset",
			header: rateResetHeader,
			want:   now.Add(10 * time.Minute),
		},
		{
			name:   "unrelated forbidden response",
			header: unrelatedResetHeader,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := &githubapp.StatusError{StatusCode: http.StatusForbidden, Header: tt.header}
			assert.Equal(t, tt.want, err.RetryDeadline(now))
		})
	}
}
