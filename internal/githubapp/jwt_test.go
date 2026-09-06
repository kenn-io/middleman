package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"go.kenn.io/forge/githubapp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

// verifyJWT checks sig and returns the decoded claims. Verification is
// independent of githubapp.SignAppJWT: it recomputes the RS256 signature check
// from the raw segments.
func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.NoError(t, rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig))
	var header map[string]any
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(headerJSON, &header))
	assert.Equal(t, "RS256", header["alg"])
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(claimsJSON, &claims))
	return claims
}

func TestSignAppJWT(t *testing.T) {
	t.Parallel()
	key := generateTestKey(t)
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	token, err := githubapp.SignAppJWT(4321, key, now)
	require.NoError(t, err)

	claims := verifyJWT(t, token, &key.PublicKey)
	iat, ok := claims["iat"].(float64)
	require.True(t, ok)
	exp, ok := claims["exp"].(float64)
	require.True(t, ok)
	assert := assert.New(t)
	assert.Equal("4321", claims["iss"])
	assert.Equal(now.Add(-time.Minute).Unix(), int64(iat))
	assert.Equal(int64(exp), now.Add(8*time.Minute).Unix())
	// GitHub rejects JWTs whose lifetime exceeds 10 minutes.
	assert.LessOrEqual(int64(exp)-int64(iat), int64(600))
}

func TestSignAppJWTRejectsBadInput(t *testing.T) {
	t.Parallel()
	key := generateTestKey(t)
	now := time.Now()

	_, err := githubapp.SignAppJWT(0, key, now)
	require.ErrorContains(t, err, "app id")
	_, err = githubapp.SignAppJWT(1, nil, now)
	require.ErrorContains(t, err, "private key")
}

func TestParsePrivateKeyFormats(t *testing.T) {
	t.Parallel()
	key := generateTestKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes})

	tests := []struct {
		name    string
		pem     []byte
		wantErr string
	}{
		{name: "pkcs1 as issued by github", pem: pkcs1},
		{name: "pkcs8 converted key", pem: pkcs8},
		{name: "not pem", pem: []byte("not a key"), wantErr: "no PEM block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, err := githubapp.ParsePrivateKey(tt.pem)
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, parsed.Equal(key))
		})
	}
}

func TestNewManifestValidation(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	_, err := NewManifest("", "", "http://127.0.0.1:1/callback")
	require.ErrorContains(err, "name is required")

	_, err = NewManifest(strings.Repeat("x", 35), "", "http://127.0.0.1:1/callback")
	require.ErrorContains(err, "34 character limit")

	m, err := NewManifest("kenn-forge-test", "", "http://127.0.0.1:9/callback")
	require.NoError(err)
	assert := assert.New(t)
	assert.False(m.Public)
	assert.False(m.HookAttributes.Active)
	assert.Equal(DefaultHomepageURL, m.HookAttributes.URL)
	assert.Empty(m.DefaultEvents)
	assert.Equal("http://127.0.0.1:9/callback", m.RedirectURL)
	assert.Equal(DefaultHomepageURL, m.URL)
	manifestJSON, err := m.JSON()
	require.NoError(err)
	var encoded struct {
		URL            string `json:"url"`
		HookAttributes struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
		} `json:"hook_attributes"`
	}
	require.NoError(json.Unmarshal([]byte(manifestJSON), &encoded))
	assert.Equal(DefaultHomepageURL, encoded.URL)
	assert.Equal(DefaultHomepageURL, encoded.HookAttributes.URL)
	assert.False(encoded.HookAttributes.Active)
	// Sync needs to read repo contents and PRs.
	assert.Equal("read", m.DefaultPermissions["contents"])
	assert.Equal("read", m.DefaultPermissions["pull_requests"])
	// The app must stay read-only: every mutation rides the user's own
	// credential chain (see tokenauth.WithMutationAuth), so a write
	// level here would be standing unused privilege.
	for scope, level := range m.DefaultPermissions {
		assert.Equal("read", level, "permission %s must be read-only", scope)
	}
}

func TestRandomAppNameFitsGitHubLimit(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	name, err := RandomAppName()
	require.NoError(err)
	assert.LessOrEqual(len(name), maxAppNameLength)
	other, err := RandomAppName()
	require.NoError(err)
	assert.NotEqual(name, other)
}
