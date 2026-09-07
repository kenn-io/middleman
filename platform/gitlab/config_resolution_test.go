package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	ghsync "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
)

func TestConfiguredGlobResolutionIncludesArchivedProjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[
			{"id": 1, "path": "one", "path_with_namespace": "kenn-forge/one", "archived": false},
			{"id": 2, "path": "old", "path_with_namespace": "kenn-forge/old", "archived": true}
		]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	registry, err := platform.NewRegistry(client)
	require.NoError(err)

	status, refs, err := ghsync.ResolveConfiguredRepoWithRegistry(
		context.Background(), registry, config.Repo{
			Platform:     "gitlab",
			PlatformHost: "gitlab.example.com",
			Owner:        "kenn-forge",
			Name:         "*",
		},
	)
	require.NoError(err)
	assert.Equal(2, status.MatchedRepoCount)
	require.Len(refs, 2)

	byName := make(map[string]ghsync.RepoRef, len(refs))
	for _, ref := range refs {
		byName[ref.Name] = ref
	}
	require.Contains(byName, "old")
	assert.True(byName["old"].Archived,
		"archived GitLab project must expand from the glob as archive-only")
	require.Contains(byName, "one")
	assert.False(byName["one"].Archived)
}
