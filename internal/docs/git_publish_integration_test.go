//go:build integration

package docs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/testutil/gitsafe"
)

type gitRepo struct {
	*gitfixture.Repository
	registry *Registry
	folderID string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	repo := gitfixture.NewRepository(t, true)
	reg := NewRegistry([]config.DocFolder{
		{ID: "f", Name: "F", Path: repo.Dir},
	}, WithGitRunner(gitsafe.Runner()))
	return &gitRepo{Repository: repo, registry: reg, folderID: "f"}
}

func newGitRepoNoUpstream(t *testing.T) *gitRepo {
	t.Helper()
	repo := gitfixture.NewRepository(t, false)
	reg := NewRegistry([]config.DocFolder{
		{ID: "f", Name: "F", Path: repo.Dir},
	}, WithGitRunner(gitsafe.Runner()))
	return &gitRepo{Repository: repo, registry: reg, folderID: "f"}
}

func TestIntegrationGitChangesNotARepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	reg := NewRegistry(
		[]config.DocFolder{{ID: "f", Name: "F", Path: dir}},
		WithGitRunner(gitsafe.Runner()),
	)

	res, err := reg.GitChanges(context.Background(), "f")

	require.NoError(err)
	assert.False(res.IsRepo)
	assert.Empty(res.Changes)
}

func TestIntegrationGitChangesEmptyRepo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)

	res, err := g.registry.GitChanges(context.Background(), g.folderID)

	require.NoError(err)
	assert.True(res.IsRepo)
	assert.Empty(res.Changes)
	assert.Equal("main", res.Branch)
	assert.Equal("origin/main", res.Upstream)
}

func TestIntegrationGitChangesIncludesUntrackedAndModifiedMarkdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	g.Write(t, "seed.md", "seed updated\n")
	g.Write(t, "code.go", "package x\n")

	res, err := g.registry.GitChanges(context.Background(), g.folderID)

	require.NoError(err)
	assert.True(res.IsRepo)
	gotPaths := make([]string, 0, len(res.Changes))
	for _, c := range res.Changes {
		gotPaths = append(gotPaths, c.Path)
	}
	assert.ElementsMatch([]string{"new.md", "seed.md"}, gotPaths)
	assert.Equal(1, res.IgnoredNonMarkdownCount)
	assert.Equal(suggestedCommitMessage(res.Changes), res.SuggestedMessage)
}

func TestIntegrationGitChangesNoUpstream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepoNoUpstream(t)

	res, err := g.registry.GitChanges(context.Background(), g.folderID)

	require.NoError(err)
	assert.Empty(res.Upstream)
	assert.Equal("main", res.Branch)
}

func TestIntegrationGitPublishHappyPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	g.Write(t, "seed.md", "seed updated\n")

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: update 2 files\n\n- new.md\n- seed.md\n")

	require.NoError(err)
	assert.NotEmpty(res.Commit)
	assert.GreaterOrEqual(len(res.Commit), 40)
	assert.NotEmpty(res.ShortCommit)
	assert.Equal("main", res.Branch)
	assert.Equal("origin/main", res.Upstream)
	assert.True(res.Pushed)
	assert.Len(res.Files, 2)
	head := strings.TrimSpace(string(gitfixture.Run(t, g.Remote, "rev-parse", "main")))
	assert.Equal(res.Commit, head)
}

func TestIntegrationGitPublishPushesConfiguredUpstreamDespitePushDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	backup := t.TempDir()
	gitfixture.Run(t, backup, "init", "--bare")
	gitfixture.Run(t, g.Dir, "remote", "add", "backup", backup)
	gitfixture.Run(t, g.Dir, "push", "backup", "main:main")
	backupInitial := strings.TrimSpace(string(gitfixture.Run(t, backup, "rev-parse", "main")))
	gitfixture.Run(t, g.Dir, "config", "remote.pushDefault", "backup")
	gitfixture.Run(t, g.Dir, "config", "push.default", "current")
	g.Write(t, "new.md", "# new\n")

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: explicit upstream")

	require.NoError(err)
	originHead := strings.TrimSpace(string(gitfixture.Run(t, g.Remote, "rev-parse", "main")))
	assert.Equal(res.Commit, originHead)
	backupHead := strings.TrimSpace(string(gitfixture.Run(t, backup, "rev-parse", "main")))
	assert.Equal(backupInitial, backupHead)
}

func TestIntegrationGitPublishRefusesEmptyMessage(t *testing.T) {
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "   \n\t")

	require.ErrorIs(t, err, ErrEmptyMessage)
}

func TestIntegrationGitPublishRefusesNoMarkdownChanges(t *testing.T) {
	g := newGitRepo(t)
	g.Write(t, "code.go", "package x\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: nothing")

	require.ErrorIs(t, err, ErrNoMarkdownChanges)
}

func TestIntegrationGitPublishRefusesNotARepo(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry([]config.DocFolder{{ID: "f", Name: "F", Path: dir}})

	_, err := reg.GitPublish(context.Background(), "f", "docs: x")

	require.ErrorIs(t, err, ErrNotAGitRepo)
}

func TestIntegrationGitPublishRefusesIndexNotCleanUnrelatedStaged(t *testing.T) {
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	g.Write(t, "code.go", "package x\n")
	g.Stage(t, "code.go")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.ErrorIs(t, err, ErrIndexNotClean)
}

func TestIntegrationGitPublishRefusesIndexNotCleanPartiallyStaged(t *testing.T) {
	g := newGitRepo(t)
	g.Write(t, "partial.md", "v1\n")
	g.Stage(t, "partial.md")
	g.Write(t, "partial.md", "v2\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.ErrorIs(t, err, ErrIndexNotClean)
}

func TestIntegrationGitPublishRefusesConflict(t *testing.T) {
	g := newGitRepo(t)
	gitfixture.Run(t, g.Dir, "checkout", "-b", "side")
	g.Write(t, "seed.md", "side version\n")
	gitfixture.Run(t, g.Dir, "commit", "-am", "side")
	gitfixture.Run(t, g.Dir, "checkout", "main")
	g.Write(t, "seed.md", "main version\n")
	gitfixture.Run(t, g.Dir, "commit", "-am", "main")
	out, _, mergeErr := gitsafe.Runner().Run(t.Context(), g.Dir, nil, "merge", "side")
	require.Error(t, mergeErr, "expected merge conflict, got clean merge: %s", out)

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.ErrorIs(t, err, ErrConflict)
}

func TestIntegrationGitPublishRefusesNoUpstream(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepoNoUpstream(t)
	g.Write(t, "new.md", "# new\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var noUpstream *NoUpstreamError
	require.ErrorAs(err, &noUpstream)
	assert.Equal("main", noUpstream.Branch)
	assert.Equal("git push -u origin main", noUpstream.SuggestedCommand)
}

func TestIntegrationGitPublishStagesRenamePair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	gitfixture.Run(t, g.Dir, "mv", "seed.md", "renamed.md")

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: rename")

	require.NoError(err)
	out := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "ls-tree", "--name-only", res.Commit)))
	assert.NotContains(out, "seed.md")
	assert.Contains(out, "renamed.md")
}

func TestIntegrationGitPublishStagesWorktreeRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	require.NoError(os.Rename(filepath.Join(g.Dir, "seed.md"), filepath.Join(g.Dir, "moved.md")))

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: rename in worktree")

	require.NoError(err)
	out := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "ls-tree", "--name-only", res.Commit)))
	assert.NotContains(out, "seed.md")
	assert.Contains(out, "moved.md")
	renames := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "show", "--name-status", "-M", "--format=", res.Commit)))
	assert.Contains(renames, "R")
}

func TestIntegrationGitPublishPushFailedAfterCommit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	gitfixture.Run(t, g.Dir, "remote", "set-url", "origin", "/does/not/exist")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var pushFailed *PushFailedAfterCommitError
	require.ErrorAs(err, &pushFailed)
	assert.NotEmpty(pushFailed.Commit)
	assert.NotEmpty(pushFailed.Stderr)
	assert.NotContains(pushFailed.Stderr, "exit status")
	head := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD")))
	assert.Equal(head, pushFailed.Commit)
}

func TestIntegrationGitPublishDoesNotRunDocsRepoHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hookBody := "#!/bin/sh\necho hooked > \"" + marker + "\"\nexit 1\n"
	hookDir := filepath.Join(g.Dir, ".git", "hooks")
	require.NoError(os.MkdirAll(hookDir, 0o755))
	for _, name := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push"} {
		require.NoError(os.WriteFile(filepath.Join(hookDir, name), []byte(hookBody), 0o755))
	}

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.NoError(err)
	assert.True(res.Pushed)
	assert.NoFileExists(marker, "docs repo hook executed during publish")
}

func TestIntegrationGitPublishIgnoresRepoHooksPathOverride(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooksPath := t.TempDir()
	hookBody := "#!/bin/sh\necho hooked > \"" + marker + "\"\nexit 1\n"
	require.NoError(os.WriteFile(filepath.Join(hooksPath, "pre-commit"), []byte(hookBody), 0o755))
	gitfixture.Run(t, g.Dir, "config", "core.hooksPath", hooksPath)

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.NoError(err)
	assert.True(res.Pushed)
	assert.NoFileExists(marker, "core.hooksPath hook executed during publish")
}

func TestIntegrationGitPublishRejectsCommandBearingLocalConfig(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"clean filter", "filter.evil.clean", "/bin/sh -c evil"},
		{"smudge filter", "filter.evil.smudge", "/bin/sh -c evil"},
		{"process filter", "filter.lfs.process", "git-lfs filter-process"},
		{"gpg program", "gpg.program", "/tmp/evil"},
		{"ssh command", "core.sshCommand", "/tmp/evil"},
		{"credential helper", "credential.helper", "/tmp/evil"},
		{"external diff", "diff.evil.command", "/tmp/evil"},
		{"textconv", "diff.evil.textconv", "/tmp/evil"},
		{"commit signing on", "commit.gpgsign", "true"},
		{"remote receive-pack", "remote.origin.receivepack", "/tmp/evil"},
		{"remote upload-pack", "remote.origin.uploadpack", "/tmp/evil"},
		{"remote vcs helper", "remote.origin.vcs", "evil"},
		{"core gitProxy", "core.gitProxy", "/tmp/evil"},
		{"core askPass", "core.askPass", "/tmp/evil"},
		{"include path", "include.path", "../evil.cfg"},
		{"includeIf path", "includeIf.gitdir:/.path", "../evil.cfg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			g := newGitRepo(t)
			g.Write(t, "new.md", "# new\n")
			gitfixture.Run(t, g.Dir, "config", tc.key, tc.value)

			_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

			var unsafe *UnsafeGitConfigError
			require.ErrorAs(err, &unsafe)
			assert.NotEmpty(unsafe.Entries)
		})
	}
}

func TestIntegrationGitPublishRejectsIncludedCommandBearingConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// The attack the include gate exists for: the directive itself looks
	// harmless, but the included file enables a signing program that a
	// later `git commit` would execute.
	included := filepath.Join(t.TempDir(), "evil.cfg")
	require.NoError(os.WriteFile(included, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /tmp/evil\n"), 0o644))
	gitfixture.Run(t, g.Dir, "config", "include.path", included)

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.NotEmpty(unsafe.Entries)
}

func TestIntegrationGitPublishRejectsPushTargetInsideDocsFolder(t *testing.T) {
	cases := []struct {
		name string
		url  func(g *gitRepo) string
	}{
		{"relative path", func(g *gitRepo) string { return "./evil.git" }},
		{"absolute path", func(g *gitRepo) string { return filepath.Join(g.Dir, "evil.git") }},
		{"file URL", func(g *gitRepo) string { return "file://" + filepath.Join(g.Dir, "evil.git") }},
		{"percent-encoded file URL", func(g *gitRepo) string {
			// Git decodes %65 to 'e' and pushes into evil.git; the
			// containment check decodes via net/url so it compares the
			// same path git resolves rather than the escaped literal.
			return "file://" + filepath.Join(g.Dir, "%65vil.git")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			g := newGitRepo(t)
			g.Write(t, "new.md", "# new\n")
			gitfixture.Run(t, g.Dir, "init", "--bare", "evil.git")
			marker := filepath.Join(t.TempDir(), "hook-ran")
			hook := "#!/bin/sh\necho hooked > \"" + marker + "\"\nexit 0\n"
			require.NoError(os.MkdirAll(filepath.Join(g.Dir, "evil.git", "hooks"), 0o755))
			require.NoError(os.WriteFile(filepath.Join(g.Dir, "evil.git", "hooks", "pre-receive"), []byte(hook), 0o755))
			gitfixture.Run(t, g.Dir, "remote", "set-url", "origin", tc.url(g))
			head := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD")))

			_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

			var unsafe *UnsafeGitConfigError
			require.ErrorAs(err, &unsafe)
			assert.NoFileExists(marker, "in-folder remote pre-receive hook executed")
			assert.Equal(head, strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD"))),
				"publish committed before rejecting the push target")
		})
	}
}

func TestIntegrationGitPublishRejectsPushInsteadOfRewriteIntoDocsFolder(t *testing.T) {
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	gitfixture.Run(t, g.Dir, "init", "--bare", "evil.git")
	// The remote URL itself looks like a safe network transport; the
	// repo-local rewrite redirects the push into the folder.
	gitfixture.Run(t, g.Dir, "remote", "set-url", "origin", "https://docs.example.invalid/repo.git")
	gitfixture.Run(t, g.Dir, "config", "url../evil.git.pushInsteadOf", "https://docs.example.invalid/repo.git")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
}

func TestIntegrationGitPublishRejectsMixedLocalAndNetworkPushURLs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// One push invocation would contact both URLs; the local
	// receive-pack hardening cannot be applied per-URL, so the set is
	// refused before anything is committed.
	gitfixture.Run(t, g.Dir, "config", "--add", "remote.origin.pushurl", g.Remote)
	gitfixture.Run(t, g.Dir, "config", "--add", "remote.origin.pushurl", "ssh://git@docs.example.invalid/repo.git")
	head := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD")))

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.Equal(head, strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "rev-parse", "HEAD"))),
		"publish committed before rejecting the mixed push urls")
}

func TestIntegrationGitPublishRejectsRemoteHelperPushTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "helper-ran")
	gitfixture.Run(t, g.Dir, "remote", "set-url", "origin", `ext::sh -c "touch `+marker+`"`)

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.NoFileExists(marker, "ext:: remote helper executed during publish")
}

func TestIntegrationGitPublishNeutralizesLocalRemoteReceiveHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// Local-path remotes outside the docs folder remain supported, but
	// the target repo's receive-side hooks must not run: receive-pack
	// executes on this machine and a hook would be arbitrary code. The
	// hook exits 1 so, if it ran, the push itself would also fail.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\necho hooked > \"" + marker + "\"\nexit 1\n"
	require.NoError(os.WriteFile(filepath.Join(g.Remote, "hooks", "pre-receive"), []byte(hook), 0o755))

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.NoError(err)
	assert.True(res.Pushed)
	assert.NoFileExists(marker, "local remote pre-receive hook executed during publish")
	assert.Equal(res.Commit, strings.TrimSpace(string(gitfixture.Run(t, g.Remote, "rev-parse", "main"))))
}

func TestIntegrationGitPublishRejectsFilterAttributes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// Even when the driver is configured globally (here it is undefined),
	// opting paths into a filter marks the repo as LFS-style and is refused.
	g.Write(t, ".gitattributes", "*.md filter=lfs diff=lfs\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.NotEmpty(unsafe.Entries)
}

func TestIntegrationGitPublishRejectsSubdirectoryFilterAttributes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "sub/new.md", "# new\n")
	// A .gitattributes nested below the root opts sub/*.md into a filter.
	// A root-only scan would miss this; check-attr resolves it per path.
	g.Write(t, "sub/.gitattributes", "*.md filter=lfs\n")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.NotEmpty(unsafe.Entries)
}

func TestIntegrationGitPublishGatesAttributesBeforeStatusRunsFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	runner := gitsafe.MutableRunner(t)
	g.registry = NewRegistry(
		[]config.DocFolder{{ID: g.folderID, Name: "F", Path: g.Dir}},
		WithGitRunner(runner),
	)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	// Simulate a globally-installed clean filter, as git-lfs would be on a
	// victim's machine. The repo only chooses to route paths through it.
	_, stderr, err := runner.Run(
		t.Context(), g.Dir, nil, "config", "--global", "filter.evil.clean",
		"sh -c 'echo ran > \""+marker+"\"; cat'",
	)
	require.NoError(err, string(stderr))
	_, stderr, err = runner.Run(
		t.Context(), g.Dir, nil, "config", "--global", "filter.evil.smudge", "cat",
	)
	require.NoError(err, string(stderr))
	// Attacker-controlled repo attributes opt markdown into that filter.
	g.Write(t, ".gitattributes", "*.md filter=evil\n")
	// Modify a tracked markdown to the same byte length as the committed
	// blob and backdate it, so git must rehash (running the clean filter)
	// to detect the change during `git status`.
	g.Write(t, "seed.md", "xxxx\n")
	old := time.Unix(1_000_000_000, 0)
	require.NoError(os.Chtimes(filepath.Join(g.Dir, "seed.md"), old, old))

	_, err = g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
	assert.NoFileExists(marker, "clean filter ran during status before the attribute gate")
}

func TestIntegrationGitStatusRejectsFilterAttributes(t *testing.T) {
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, ".gitattributes", "*.md filter=lfs\n")

	_, err := g.registry.GitStatus(context.Background(), g.folderID)

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
}

func TestIntegrationGitChangesRejectsFilterAttributes(t *testing.T) {
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "sub/.gitattributes", "*.md diff=evil\n")
	g.Write(t, "sub/new.md", "# new\n")

	_, err := g.registry.GitChanges(context.Background(), g.folderID)

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
}

func TestIntegrationGitChangesRejectsCommandBearingLocalConfig(t *testing.T) {
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// The preview must apply the same config gate as publish so a folder
	// with unsafe local config cannot preview as publishable and only
	// fail on submit.
	gitfixture.Run(t, g.Dir, "config", "gpg.program", "/tmp/evil")

	_, err := g.registry.GitChanges(context.Background(), g.folderID)

	var unsafe *UnsafeGitConfigError
	require.ErrorAs(err, &unsafe)
}

func TestIntegrationGitPublishAllowsBenignLocalConfig(t *testing.T) {
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	// Signing explicitly off and benign attributes must not trip the gate.
	gitfixture.Run(t, g.Dir, "config", "commit.gpgsign", "false")
	gitfixture.Run(t, g.Dir, "config", "tag.gpgsign", "false")
	g.Write(t, ".gitattributes", "*.md text=auto eol=lf\n")

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.NoError(err)
	require.True(res.Pushed)
}

func TestIntegrationGitStatusDoesNotRunRepoFsmonitor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	monitor := filepath.Join(t.TempDir(), "fsmonitor")
	require.NoError(os.WriteFile(monitor, []byte("#!/bin/sh\necho ran > \""+marker+"\"\nexit 1\n"), 0o755))
	gitfixture.Run(t, g.Dir, "config", "core.fsmonitor", monitor)

	// GitStatus runs `git status`, which honors core.fsmonitor.
	_, err := g.registry.GitStatus(context.Background(), g.folderID)

	require.NoError(err)
	assert.NoFileExists(marker, "core.fsmonitor program executed during git status")
}

func TestIntegrationGitPublishBlocksExtRemoteHelperOnPush(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "helper-ran")
	helper := filepath.Join(t.TempDir(), "evil")
	require.NoError(os.WriteFile(helper, []byte("#!/bin/sh\necho ran > \""+marker+"\"\n"), 0o755))
	// An attacker-controlled repo config can opt the ext transport back in
	// (modern git blocks it by default) and point a remote at an arbitrary
	// command, which push would execute without our protocol.ext override.
	gitfixture.Run(t, g.Dir, "config", "protocol.ext.allow", "always")
	gitfixture.Run(t, g.Dir, "remote", "set-url", "origin", "ext::"+helper)

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	require.Error(err)
	assert.NoFileExists(marker, "ext:: remote helper executed during push")
}

func TestIntegrationGitPublishCommitFailurePreservesStderr(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "new.md", "# new\n")
	gitfixture.Run(t, g.Dir, "config", "user.useConfigOnly", "true")
	gitfixture.Run(t, g.Dir, "config", "--unset", "user.email")

	_, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: x")

	var commitFailed *CommitFailedError
	require.ErrorAs(err, &commitFailed)
	assert.Contains(commitFailed.Stderr, "email")
	assert.NotContains(commitFailed.Stderr, "exit status")
}

func TestIntegrationGitPublishStagesLiteralPathspec(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := newGitRepo(t)
	g.Write(t, "weird *.md", "# weird\n")
	g.Write(t, "decoy.md", "# decoy\n")

	res, err := g.registry.GitPublish(context.Background(), g.folderID, "docs: weird")

	require.NoError(err)
	out := strings.TrimSpace(string(gitfixture.Run(t, g.Dir, "ls-tree", "--name-only", res.Commit)))
	assert.Contains(out, "weird *.md")
	assert.Contains(out, "decoy.md")
}
