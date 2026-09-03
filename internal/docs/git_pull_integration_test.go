//go:build integration

package docs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/testutil/gitfixture"
)

func TestIntegrationGitPullFastForwards(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	want := g.RemoteCommit(t, "remote.md", "# remote\n")

	res, err := g.registry.GitPull(context.Background(), g.folderID)

	require.NoError(err)
	assert.False(res.UpToDate)
	assert.Equal(want, res.Commit)
	assert.Equal(want[:7], res.ShortCommit)
	assert.Equal("main", res.Branch)
	assert.Equal("origin/main", res.Upstream)
	assert.Equal(want, strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD"))))
	// The source-only refspec fetch must still refresh the remote-tracking
	// ref via git's opportunistic update, or origin/main would go stale and
	// the branch would wrongly appear ahead of its upstream.
	assert.Equal(want, strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "origin/main"))))
	body, readErr := os.ReadFile(filepath.Join(g.Dir, "remote.md"))
	require.NoError(readErr)
	assert.Equal("# remote\n", string(body))
}

func TestIntegrationGitPullUpToDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	head := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD")))

	res, err := g.registry.GitPull(context.Background(), g.folderID)

	require.NoError(err)
	assert.True(res.UpToDate)
	assert.Equal(head, res.Commit)
	assert.Equal(head[:7], res.ShortCommit)
}

func TestIntegrationGitPullRefusesDiverged(t *testing.T) {
	g := newGitRepo(t)
	g.RemoteCommit(t, "remote.md", "remote\n")
	g.Write(t, "local.md", "local\n")
	gitfixture.Run(t, g.Dir, "add", "--", "local.md")
	gitfixture.Run(t, g.Dir, "commit", "-m", "local update")

	_, err := g.registry.GitPull(context.Background(), g.folderID)

	require.ErrorIs(t, err, ErrDiverged)
}

func TestIntegrationGitPullRefusesOverwritingDirtyWorktree(t *testing.T) {
	g := newGitRepo(t)
	g.RemoteCommit(t, "seed.md", "remote seed\n")
	g.Write(t, "seed.md", "local dirty\n")

	_, err := g.registry.GitPull(context.Background(), g.folderID)

	var pullFailed *PullFailedError
	require.ErrorAs(t, err, &pullFailed)
	assert.Contains(t, pullFailed.Stderr, "overwritten")
	// The dirty local edit must survive the refused pull.
	body, readErr := os.ReadFile(filepath.Join(g.Dir, "seed.md"))
	require.NoError(t, readErr)
	assert.Equal(t, "local dirty\n", string(body))
}

func TestIntegrationGitPullRefusesNoUpstream(t *testing.T) {
	g := newGitRepoNoUpstream(t)

	_, err := g.registry.GitPull(context.Background(), g.folderID)

	var noUpstream *NoUpstreamError
	require.ErrorAs(t, err, &noUpstream)
	assert.Contains(t, noUpstream.SuggestedCommand, "--set-upstream-to")
}

func TestIntegrationGitPullRefusesNotARepo(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry([]config.DocFolder{{ID: "f", Name: "F", Path: dir}})

	_, err := reg.GitPull(context.Background(), "f")

	require.ErrorIs(t, err, ErrNotAGitRepo)
}
