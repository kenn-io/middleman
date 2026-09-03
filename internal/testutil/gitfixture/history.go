// Package gitfixture builds synthetic Git histories for tests.
package gitfixture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/gitsafe"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// DivergenceWorktree creates a local feature branch that tracks a bare remote.
func DivergenceWorktree(t *testing.T) string {
	t.Helper()
	require := require.New(t)
	runner := gitsafe.Runner().WithConfig("init.defaultBranch", "main")
	run := func(dir string, args ...string) {
		t.Helper()
		out, stderr, err := runner.Run(t.Context(), dir, nil, args...)
		require.NoError(err, "git %v failed: %s%s", args, out, stderr)
	}

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	run(root, "init", "--bare", "--initial-branch=main", remote)
	run(root, "clone", remote, work)
	run(work, "config", "user.email", "t@test.com")
	run(work, "config", "user.name", "Test")
	require.NoError(os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o644))
	run(work, "add", ".")
	run(work, "commit", "-m", "base")
	run(work, "push", "origin", "main")
	run(work, "checkout", "-b", "feature")
	require.NoError(os.WriteFile(filepath.Join(work, "f.txt"), []byte("f1\n"), 0o644))
	run(work, "add", ".")
	run(work, "commit", "-m", "feature 1")
	run(work, "push", "-u", "origin", "feature")
	return work
}

// Run executes Git with the package test process's isolated configuration.
func Run(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	runner := gitsafe.Runner().WithConfig("init.defaultBranch", "main")
	out, stderr, err := runner.Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, "git %v failed: %s%s", args, out, stderr)
	return out
}

// SHA resolves ref in dir and returns the trimmed object ID.
func SHA(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(string(Run(t, dir, "rev-parse", ref)))
}

// AppendFileCommits adds count commits that successively replace path on ref.
// It uses one fast-import process so boundary-size histories do not exhaust
// shared subprocess capacity when Go packages run concurrently.
func AppendFileCommits(t *testing.T, dir, ref, path string, count int) {
	t.Helper()
	require := require.New(t)
	runner := gitcmd.New().WithConfig("gc.auto", "0").WithConfig("maintenance.auto", "false")
	base, err := runner.Output(t.Context(), dir, "rev-parse", ref)
	require.NoError(err)

	var stream bytes.Buffer
	parent := strings.TrimSpace(string(base))
	mark := 1
	startedAt := time.Now().UTC().Unix()
	for i := range count {
		content := fmt.Appendf(nil, "%d\n", i)
		fmt.Fprintf(&stream, "blob\nmark :%d\ndata %d\n", mark, len(content))
		stream.Write(content)
		stream.WriteByte('\n')
		blobMark := mark
		mark++

		message := fmt.Sprintf("churn %03d", i)
		fmt.Fprintf(
			&stream,
			"commit refs/heads/%s\nmark :%d\nauthor Alice <alice@example.com> %d +0000\ncommitter Alice <alice@example.com> %d +0000\ndata %d\n%s\nfrom %s\nM 100644 :%d %s\n\n",
			ref,
			mark,
			startedAt+int64(i),
			startedAt+int64(i),
			len(message),
			message,
			parent,
			blobMark,
			path,
		)
		parent = fmt.Sprintf(":%d", mark)
		mark++
	}

	out, stderr, err := runner.Run(t.Context(), dir, &stream, "fast-import", "--quiet")
	require.NoError(err, "git fast-import failed: %s%s", out, stderr)
	out, stderr, err = runner.Run(t.Context(), dir, nil, "reset", "--hard", ref)
	require.NoError(err, "git reset failed: %s%s", out, stderr)
}
