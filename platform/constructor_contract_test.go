package platform_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/forgejo"
	"go.kenn.io/forge/platform/gitea"
	"go.kenn.io/forge/platform/gitlab"
)

func TestPublicConstructorsRequireCallerTransport(t *testing.T) {
	factories := map[string]func(string, http.RoundTripper) (platform.Provider, error){
		"gitlab": func(host string, transport http.RoundTripper) (platform.Provider, error) {
			return gitlab.NewClient(host, nil, gitlab.WithTransport(transport))
		},
		"gitea": func(host string, transport http.RoundTripper) (platform.Provider, error) {
			return gitea.NewClient(host, nil, gitea.WithTransport(transport))
		},
		"forgejo": func(host string, transport http.RoundTripper) (platform.Provider, error) {
			return forgejo.NewClient(host, nil, forgejo.WithTransport(transport))
		},
	}
	for name, create := range factories {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			_, err := create("code.example.org", nil)
			require.ErrorIs(err, platform.ErrInvalidArgument)
			client, err := create("code.example.org:8443", http.DefaultTransport)
			require.NoError(err)
			assert.Equal(t, "code.example.org:8443", client.Host())
		})
	}
}
