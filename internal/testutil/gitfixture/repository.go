package gitfixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/gitsafe"
)

// Repository is a temporary Git repository for tests, optionally connected to
// a bare origin that tracks its main branch.
type Repository struct {
	Dir    string
	Remote string
}

// NewRepository creates a repository with one seed commit. When withUpstream
// is true, it also creates a bare origin and configures main to track it.
func NewRepository(t *testing.T, withUpstream bool) *Repository {
	t.Helper()
	if _, err := gitsafe.Runner().Output(t.Context(), "", "--version"); err != nil {
		t.Skip("git binary unavailable")
	}

	repo := &Repository{
		Dir:    t.TempDir(),
		Remote: t.TempDir(),
	}
	Run(t, repo.Dir, "init", "-b", "main")
	Run(t, repo.Dir, "config", "user.email", "kenn-forge-fixture@example.invalid")
	Run(t, repo.Dir, "config", "user.name", "Kenn Forge Fixture")
	Run(t, repo.Dir, "config", "commit.gpgsign", "false")
	Run(t, repo.Dir, "config", "tag.gpgsign", "false")
	Run(t, repo.Dir, "config", "core.hooksPath", ".git/hooks")
	repo.Write(t, "seed.md", "seed\n")
	Run(t, repo.Dir, "add", "seed.md")
	Run(t, repo.Dir, "commit", "-m", "seed")
	if withUpstream {
		Run(t, repo.Remote, "init", "--bare")
		Run(t, repo.Dir, "remote", "add", "origin", repo.Remote)
		Run(t, repo.Dir, "push", "-u", "origin", "main")
	}
	return repo
}

// Write writes body to rel within the repository worktree.
func (r *Repository) Write(t *testing.T, rel, body string) {
	t.Helper()
	full := filepath.Join(r.Dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
}

// Stage stages paths as literal pathspecs.
func (r *Repository) Stage(t *testing.T, paths ...string) {
	t.Helper()
	Run(t, r.Dir, append([]string{"add", "--"}, paths...)...)
}

// RemoteCommit advances Remote through a scratch clone and returns its head.
func (r *Repository) RemoteCommit(t *testing.T, rel, body string) string {
	t.Helper()
	clone := t.TempDir()
	Run(t, r.Dir, "clone", r.Remote, clone)
	Run(t, clone, "checkout", "main")
	Run(t, clone, "config", "user.email", "kenn-forge-fixture@example.invalid")
	Run(t, clone, "config", "user.name", "Kenn Forge Fixture")
	Run(t, clone, "config", "commit.gpgsign", "false")
	cloneRepo := &Repository{Dir: clone}
	cloneRepo.Write(t, rel, body)
	Run(t, clone, "add", "--", rel)
	Run(t, clone, "commit", "-m", "remote update")
	Run(t, clone, "push", "origin", "main")
	return strings.TrimSpace(string(Run(t, clone, "rev-parse", "HEAD")))
}
