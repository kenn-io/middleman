package githubapp

import (
	"crypto/rsa"
	"fmt"
	"os"

	"go.kenn.io/forge/githubapp"
)

// LoadPrivateKey is application-owned file access, outside the App primitives.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading app private key: %w", err)
	}
	return githubapp.ParsePrivateKey(data)
}
