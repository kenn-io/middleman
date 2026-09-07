package githubapp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"time"
)

// ParsePrivateKey decodes the PEM private key GitHub issues for an
// app. GitHub currently hands out PKCS#1 ("RSA PRIVATE KEY") blocks;
// PKCS#8 is accepted too so externally converted keys keep working.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app private key is %T, want RSA", parsed)
	}
	return key, nil
}

// SignAppJWT mints the short-lived RS256 JWT GitHub Apps use to call
// app-scoped endpoints (/app, /app/installations, token minting).
// iat is backdated 60s against clock drift; exp stays well inside
// GitHub's 10 minute maximum.
func SignAppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	if appID <= 0 {
		return "", fmt.Errorf("app id must be positive, got %d", appID)
	}
	if key == nil {
		return "", fmt.Errorf("app private key is required")
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(8 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encoding JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encoding JWT claims: %w", err)
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing app JWT: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}
