package gitclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/testutil/gitfake"
	"go.kenn.io/forge/internal/testutil/gitsafe"
	"go.kenn.io/forge/internal/tokenauth"
)

type mutableTestTokenSource struct {
	token       string
	invalidated int
	resolved    int
}

func (s *mutableTestTokenSource) Token(context.Context) (string, error) {
	s.resolved++
	return s.token, nil
}

func (s *mutableTestTokenSource) Invalidate(rejectedToken string) {
	if s.token != rejectedToken {
		return
	}
	s.invalidated++
	s.token = "second-token"
}

func (s *mutableTestTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "test", Host: "github.com"}}
}

type testRouteResolver struct {
	repos    map[string]tokenauth.Source
	fallback map[string]tokenauth.Source
}

func (r testRouteResolver) SourceForRepo(_, host, owner, name string) tokenauth.Source {
	return r.repos[host+"/"+owner+"/"+name]
}

func (r testRouteResolver) FallbackSource(host string) tokenauth.Source {
	return r.fallback[host]
}

func TestDescriptorCloneRequiresCredentialOnExecutingNode(t *testing.T) {
	mgr := New(t.TempDir(), testRouteResolver{})

	err := mgr.RequireCredentialRoute(
		t.Context(), "github", "github.com", "acme", "widgets",
	)

	require.ErrorIs(t, err, ErrCredentialUnavailable)
}

func TestDescriptorNetworkedGitNeverFallsBackToAnonymous(t *testing.T) {
	mgr := New(t.TempDir(), testRouteResolver{})

	_, err := mgr.gitNetworked(
		WithRequiredCredential(t.Context()), nil, "github.com", "", nil,
		"fetch",
	)

	require.ErrorIs(t, err, ErrCredentialUnavailable)
}

func TestGitOwnerRoutesSelectAndInvalidateOnlyTheirSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "credentials.txt")
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
out="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
tmp="$out.current"
helper=""
i=0
count="${GIT_CONFIG_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i:-}"
	eval "value=\${GIT_CONFIG_VALUE_$i:-}"
	if [ "$key" = "credential.helper" ]; then helper="$value"; fi
	i=$((i + 1))
done
run_credential_helper "$helper" get > "$tmp"
password="$(sed -n 's/^password=//p' "$tmp")"
echo "$password" >> "$out"
if [ "$password" = "first-token" ]; then
	echo "fatal: Authentication failed" >&2
	exit 128
fi
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)

	acme := &mutableTestTokenSource{token: "first-token"}
	example := &mutableTestTokenSource{token: "token-b"}
	fallback := &mutableTestTokenSource{token: "fallback-token"}
	mgr := New(t.TempDir(), testRouteResolver{
		repos: map[string]tokenauth.Source{
			"github.com/acme/widgets":  acme,
			"github.com/example/tools": example,
		},
		fallback: map[string]tokenauth.Source{"github.com": fallback},
	})

	_, err := mgr.RunGitForRepo(
		t.Context(), "github", "github.com", "acme", "widgets", "", "fetch",
	)
	require.NoError(err)
	_, err = mgr.RunGitForRepo(
		t.Context(), "github", "github.com", "example", "tools", "", "fetch",
	)
	require.NoError(err)
	_, err = mgr.RunGitForHost(t.Context(), "github.com", "", "fetch")
	require.NoError(err)

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	assert.Equal([]string{
		"first-token", "second-token", "token-b", "fallback-token",
	}, strings.Split(strings.TrimSpace(string(data)), "\n"))
	assert.Equal(1, acme.invalidated)
	assert.Zero(example.invalidated)
	assert.Zero(fallback.invalidated)
}

func TestRunGitForRepoRejectsRewrittenOriginBeforeResolvingCredential(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
if [ "$1" = "config" ] && [ "$2" = "--local" ]; then exit 1; fi
if [ "$1" = "config" ] && [ "$2" = "--get-all" ] && [ "$3" = "remote.origin.url" ]; then
	echo "https://github.com/other/repo.git"
	exit 0
fi
if [ "$1" = "config" ] && [ "$2" = "--get-all" ] && [ "$3" = "remote.origin.pushurl" ]; then
	exit 1
fi
exit 0
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := &mutableTestTokenSource{token: "secret-token"}
	mgr := New(t.TempDir(), testRouteResolver{
		repos: map[string]tokenauth.Source{"github.com/acme/widgets": source},
	})

	_, err := mgr.RunGitForRepo(
		t.Context(), "github", "github.com", "acme", "widgets", dir, "fetch",
	)
	require.Error(err)
	assert.Contains(t, err.Error(), "validate remote.origin.url")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.Zero(t, source.invalidated)
}

func TestRunGitForRepoRejectsRepositoryLocalURLRewrite(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
if [ "$1" = "config" ] && [ "$2" = "--local" ]; then
	echo "url.https://evil.example/.insteadOf https://github.com/"
	exit 0
fi
exit 1
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	mgr := New(t.TempDir(), testRouteResolver{
		repos: map[string]tokenauth.Source{
			"github.com/acme/widgets": &mutableTestTokenSource{token: "secret-token"},
		},
	})

	_, err := mgr.RunGitForRepo(
		t.Context(), "github", "github.com", "acme", "widgets", dir, "fetch",
	)
	require.Error(err)
	assert.Contains(t, err.Error(), "repository-local URL rewrites")
}

func TestRunGitForRepoRemoteValidatesSelectedRemote(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
if [ "$1" = "config" ] && [ "$2" = "--local" ]; then exit 1; fi
if [ "$1" = "config" ] && [ "$2" = "--get-all" ]; then
	case "$3" in
	remote.upstream.url) echo "https://github.com/acme/widgets.git"; exit 0 ;;
	remote.upstream.pushurl) exit 1 ;;
	esac
fi
exit 0
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := &mutableTestTokenSource{token: "secret-token"}
	mgr := New(t.TempDir(), testRouteResolver{repos: map[string]tokenauth.Source{
		"github.com/acme/widgets": source,
	}})

	_, err := mgr.RunGitForRepoRemote(
		t.Context(), "github", "github.com", "acme", "widgets",
		"upstream", dir, "fetch", "upstream",
	)

	require.NoError(err)
	assert.Equal(t, 1, source.resolved)
}

func TestRunGitForNamedRemoteUsesRemoteRepositoryCredential(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
if [ "$1" = "config" ] && [ "$2" = "--local" ]; then exit 1; fi
if [ "$1" = "config" ] && [ "$2" = "--get-all" ]; then
	case "$3" in
	remote.origin.url) echo "https://github.com/forker/widgets.git"; exit 0 ;;
	remote.origin.pushurl) exit 1 ;;
	esac
fi
exit 0
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	canonical := &mutableTestTokenSource{token: "canonical-token"}
	fork := &mutableTestTokenSource{token: "fork-token"}
	mgr := New(t.TempDir(), testRouteResolver{repos: map[string]tokenauth.Source{
		"github.com/acme/widgets":   canonical,
		"github.com/forker/widgets": fork,
	}})

	_, err := mgr.RunGitForNamedRemote(
		t.Context(), "github", "github.com", "acme", "widgets",
		"origin", dir, "fetch", "origin",
	)

	require.NoError(err)
	assert.Zero(t, canonical.resolved)
	assert.Equal(t, 1, fork.resolved)
}

func TestRunGitForNamedRemoteRejectsInsecurePushURLForLocalFetchURL(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
if [ "$1" = "config" ] && [ "$2" = "--local" ]; then exit 1; fi
if [ "$1" = "config" ] && [ "$2" = "--get-all" ]; then
	case "$3" in
	remote.origin.url) echo "/tmp/widgets.git"; exit 0 ;;
	remote.origin.pushurl) echo "http://git.example.test/acme/widgets.git"; exit 0 ;;
	esac
fi
exit 0
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := &mutableTestTokenSource{token: "secret-token"}
	mgr := New(t.TempDir(), testRouteResolver{repos: map[string]tokenauth.Source{
		"git.example.test/acme/widgets": source,
	}})

	_, err := mgr.RunGitForNamedRemote(
		t.Context(), "gitea", "git.example.test", "acme", "widgets",
		"origin", dir, "push", "origin", "HEAD:feature",
	)

	require.ErrorContains(err, "allow_insecure = true")
	require.Zero(source.resolved)
}

func TestRunGitForNamedRemoteAllowsAnonymousLocalURLRewrite(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	const hostedURL = "https://github.com/acme/widgets.git"
	runner := gitsafe.MutableRunner(t).WithConfig("init.defaultBranch", "main")
	_, err := runner.Output(t.Context(), root, "init", "--bare", remote)
	require.NoError(err)
	_, err = runner.Output(t.Context(), root, "init", repo)
	require.NoError(err)
	_, err = runner.Output(t.Context(), repo, "remote", "add", "origin", hostedURL)
	require.NoError(err)
	_, err = runner.Output(t.Context(), repo, "config", "url."+remote+".insteadOf", hostedURL)
	require.NoError(err)
	mgr := New(t.TempDir(), nil)

	_, err = mgr.RunGitForNamedRemote(
		t.Context(), "github", "github.com", "acme", "widgets",
		"origin", repo, "fetch", "origin",
	)

	require.NoError(err)
}

func TestRunGitForRemoteRequiresAcknowledgedHTTPTransport(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const (
		platform = "gitea"
		host     = "git.example.test"
		remote   = "http://git.example.test/acme/widgets.git"
	)
	source := &mutableTestTokenSource{token: "secret-token"}
	mgr := New(t.TempDir(), testRouteResolver{repos: map[string]tokenauth.Source{
		host + "/acme/widgets": source,
	}})

	_, err := mgr.RunGitForRemote(
		t.Context(), platform, host, remote, dir, "fetch", remote,
	)
	require.ErrorContains(err, "allow_insecure = true")
	assert.Zero(t, source.resolved)

	mgr.SetAllowInsecureHTTP(platform, host, true)
	_, err = mgr.RunGitForRemote(
		t.Context(), platform, host, remote, dir, "fetch", remote,
	)
	require.NoError(err)
	assert.Equal(t, 1, source.resolved)
}

func TestGitNetworkedResolvesTokenSourceForEachCall(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "credentials.txt")
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
out="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
i=0
count="${GIT_CONFIG_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i:-}"
	eval "value=\${GIT_CONFIG_VALUE_$i:-}"
	if [ "$key" = "credential.helper" ]; then
		run_credential_helper "$value" get >> "$out"
		echo "---" >> "$out"
	fi
	i=$((i + 1))
done
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)

	source := &mutableTestTokenSource{token: "first-token"}
	mgr := New(t.TempDir(), HostSources{"github.com": source})

	_, err := mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch")
	require.NoError(err)
	source.token = "second-token"
	_, err = mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch")
	require.NoError(err)

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	credentials := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Equal([]string{
		"username=x-access-token",
		"password=first-token",
		"---",
		"username=x-access-token",
		"password=second-token",
		"---",
	}, credentials)
}

func TestGitNetworkedResolvesTokenFileSourceForEachCall(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "credentials.txt")
	tokenPath := filepath.Join(dir, "token")
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
out="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
i=0
count="${GIT_CONFIG_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i:-}"
	eval "value=\${GIT_CONFIG_VALUE_$i:-}"
	if [ "$key" = "credential.helper" ]; then
		run_credential_helper "$value" get >> "$out"
		echo "---" >> "$out"
	fi
	i=$((i + 1))
done
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)

	require.NoError(os.WriteFile(tokenPath, []byte("first-token\n"), 0o600))
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "test", Host: "github.com"},
		Candidates: []tokenauth.Candidate{{
			Kind:     tokenauth.SourceKindFile,
			FilePath: tokenPath,
		}},
	}, tokenauth.Options{})
	mgr := New(t.TempDir(), HostSources{"github.com": source})

	_, err := mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch")
	require.NoError(err)
	require.NoError(os.WriteFile(tokenPath, []byte("second-token\n"), 0o600))
	_, err = mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch")
	require.NoError(err)

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	credentials := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Equal([]string{
		"username=x-access-token",
		"password=first-token",
		"---",
		"username=x-access-token",
		"password=second-token",
		"---",
	}, credentials)
}

func TestGitRetriesAuthFailureAfterInvalidatingTokenSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "credentials.txt")
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
out="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
tmp="$out.current"
helper=""
i=0
count="${GIT_CONFIG_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i:-}"
	eval "value=\${GIT_CONFIG_VALUE_$i:-}"
	if [ "$key" = "credential.helper" ]; then
		helper="$value"
	fi
	i=$((i + 1))
done
run_credential_helper "$helper" get > "$tmp"
cat "$tmp" >> "$out"
echo "---" >> "$out"
password="$(sed -n 's/^password=//p' "$tmp")"
if [ "$password" = "first-token" ]; then
	echo "fatal: Authentication failed for 'https://github.com/acme/widgets.git/'" >&2
	exit 128
fi
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)

	source := &mutableTestTokenSource{token: "first-token"}
	mgr := New(t.TempDir(), HostSources{"github.com": source})

	_, err := mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch", "origin")
	require.NoError(err)

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	credentials := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Equal(1, source.invalidated)
	assert.Equal([]string{
		"username=x-access-token",
		"password=first-token",
		"---",
		"username=x-access-token",
		"password=second-token",
		"---",
	}, credentials)
}

func TestCloneBareRetriesAuthFailureAfterCleaningPartialClone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "credentials.txt")
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
`+gitfake.CredentialHelperRunner+`
if [ "${1:-}" != "clone" ]; then
	exit 0
fi

out="${KENN_FORGE_TEST_GIT_CAPTURE:?}"
dest=""
for arg in "$@"; do
	dest="$arg"
done
: "${dest:?}"
if [ -e "$dest" ]; then
	echo "fatal: destination path '$dest' already exists and is not an empty directory." >&2
	exit 128
fi

helper=""
i=0
count="${GIT_CONFIG_COUNT:-0}"
while [ "$i" -lt "$count" ]; do
	eval "key=\${GIT_CONFIG_KEY_$i:-}"
	eval "value=\${GIT_CONFIG_VALUE_$i:-}"
	if [ "$key" = "credential.helper" ]; then
		helper="$value"
	fi
	i=$((i + 1))
done

tmp="$out.current"
run_credential_helper "$helper" get > "$tmp"
cat "$tmp" >> "$out"
echo "---" >> "$out"
password="$(sed -n 's/^password=//p' "$tmp")"

mkdir -p "$dest"
if [ "$password" = "first-token" ]; then
	echo partial > "$dest/partial"
	echo "fatal: Authentication failed for 'https://github.com/acme/widgets.git/'" >&2
	exit 128
fi
echo complete > "$dest/complete"
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KENN_FORGE_TEST_GIT_CAPTURE", capturePath)

	source := &mutableTestTokenSource{token: "first-token"}
	mgr := New(t.TempDir(), HostSources{"github.com": source})
	clonePath := filepath.Join(dir, "widgets.git")

	err := mgr.cloneBare(
		t.Context(), "github", "github.com", "acme", "widgets", clonePath,
		"https://github.com/acme/widgets.git",
	)
	require.NoError(err)

	data, err := os.ReadFile(capturePath)
	require.NoError(err)
	credentials := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Equal(1, source.invalidated)
	assert.Equal([]string{
		"username=x-access-token",
		"password=first-token",
		"---",
		"username=x-access-token",
		"password=second-token",
		"---",
	}, credentials)
	assert.NoFileExists(filepath.Join(clonePath, "partial"))
	assert.FileExists(filepath.Join(clonePath, "complete"))
}

func TestGitNetworkedRedactsTokenFromGitStderr(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(`#!/bin/sh
set -eu
echo "fatal: Authentication failed for 'https://x-access-token:ghp_stderr_secret@github.com/acme/widgets.git/'" >&2
exit 128
`), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	source := &mutableTestTokenSource{token: "first-token"}
	mgr := New(t.TempDir(), HostSources{"github.com": source})

	_, err := mgr.gitNetworked(t.Context(), source, "github.com", "", nil, "fetch", "origin")
	require.Error(err)

	assert.NotContains(err.Error(), "ghp_stderr_secret")
	assert.NotContains(err.Error(), "x-access-token")
	assert.Contains(err.Error(), "[REDACTED]")
}

func TestWrapGitErrorPreservesContextCancellationIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)

			err := wrapGitError(
				tc.err,
				[]byte("fatal: Authentication failed for 'https://x-access-token:ghp_context_secret@github.com/acme/widgets.git/'"),
			)

			require.ErrorIs(t, err, tc.err)
			assert.NotContains(err.Error(), "ghp_context_secret")
			assert.NotContains(err.Error(), "x-access-token")
			assert.Contains(err.Error(), "[REDACTED]")
		})
	}
}

func TestWrapGitErrorPreservesMissingTokenIdentity(t *testing.T) {
	assert := assert.New(t)

	err := wrapGitError(
		fmt.Errorf("resolve git token: %w", tokenauth.ErrMissingToken),
		[]byte("fatal: Authentication failed for 'https://x-access-token:ghp_missing_secret@github.com/acme/widgets.git/'"),
	)

	require.ErrorIs(t, err, tokenauth.ErrMissingToken)
	assert.NotContains(err.Error(), "ghp_missing_secret")
	assert.NotContains(err.Error(), "x-access-token")
	assert.Contains(err.Error(), "[REDACTED]")
}

// failingTokenSource never resolves a token, standing in for a token file that
// is briefly missing or empty mid-rotation. It counts how often the resolver
// was consulted so a test can assert local reads never touch it.
type failingTokenSource struct {
	calls int
}

func (s *failingTokenSource) Token(context.Context) (string, error) {
	s.calls++
	return "", tokenauth.ErrMissingToken
}

func (s *failingTokenSource) Invalidate(string) {}

func (s *failingTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "test", Host: "github.com"}}
}

// TestLocalReadSkipsTokenSourceDuringRotation verifies a local read against an
// already-cloned repo (rev-parse) succeeds even when the host's token source
// cannot resolve a credential. Local git never contacts the remote, so it must
// not depend on a live token — otherwise a token file briefly missing during
// rotation would break commit and diff views.
func TestLocalReadSkipsTokenSourceDuringRotation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	gitPath := filepath.Join(dir, "git")
	require.NoError(os.WriteFile(gitPath, []byte(
		"#!/bin/sh\necho deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
	), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	source := &failingTokenSource{}
	clonesDir := t.TempDir()
	mgr := New(clonesDir, HostSources{"github.com": source})
	clonePath, err := mgr.ClonePath("github", "github.com", "acme", "widgets")
	require.NoError(err)
	require.NoError(os.MkdirAll(clonePath, 0o755))

	sha, err := mgr.RevParse(t.Context(), "github", "github.com", "acme", "widgets", "HEAD")
	require.NoError(err)
	assert.Equal("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", sha)
	assert.Zero(source.calls, "local read must not resolve the token source")
}
