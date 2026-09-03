package docstest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/docs"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/gitfixture"
	"go.kenn.io/forge/internal/testutil/gitsafe"
	"go.kenn.io/forge/internal/testutil/servertest"
)

func setupDocsGitRouteServer(t *testing.T, root string) *server.Server {
	t.Helper()
	cfg := &config.Config{
		Host:       "127.0.0.1",
		Port:       8091,
		DocFolders: []config.DocFolder{{ID: "f", Name: "F", Path: root}},
	}
	registry := docs.NewRegistry(cfg.DocFolders, docs.WithGitRunner(gitsafe.Runner()))
	srv := servertest.New(t, dbtest.Open(t), nil, nil, "/", cfg, server.ServerOptions{
		DocsRegistry:                  registry,
		HostCheckAllowLoopbackAnyPort: true,
	})
	return srv
}

func TestDocsGitStatusEndpointReturnsEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/f/git", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.GitStatusResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.True(body.IsRepo)
	require.NotEmpty(body.Entries)
	assert.Equal("new.md", body.Entries[0].Path)
	assert.Equal(docs.GitStatusUntracked, body.Entries[0].Status)
}

func TestDocsGitChangesEndpointReturnsPreview(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/f/git/changes", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.GitChangesResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.True(body.IsRepo)
	assert.Equal("main", body.Branch)
	assert.Equal("origin/main", body.Upstream)
	require.Len(body.Changes, 1)
	assert.Equal("new.md", body.Changes[0].Path)
	assert.NotEmpty(body.SuggestedMessage)
}

func TestDocsGitChangesEndpointNotARepoAndUnknownFolder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupDocsGitRouteServer(t, t.TempDir())

	notRepoRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/f/git/changes", nil)
	require.Equal(http.StatusOK, notRepoRR.Code, notRepoRR.Body.String())
	var notRepo docs.GitChangesResponse
	require.NoError(json.NewDecoder(notRepoRR.Body).Decode(&notRepo))
	assert.False(notRepo.IsRepo)
	assert.NotNil(notRepo.Changes)

	missingRR := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/missing/git/changes", nil)
	assert.Equal(http.StatusNotFound, missingRR.Code, missingRR.Body.String())
}

func TestDocsGitStatusAndChangesEndpointsRejectUnsafeAttributes(t *testing.T) {
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, ".gitattributes", "*.md filter=evil\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	for _, path := range []string{
		"/api/v1/docs/folders/f/git",
		"/api/v1/docs/folders/f/git/changes",
	} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			rr := testutil.DoJSON(t, srv, http.MethodGet, path, nil)

			require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
			var problem httpapi.ProblemError
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal(httpapi.CodeBadRequest, problem.Code)
			assert.Equal("unsafeGitConfig", problem.Details["reason"])
		})
	}
}

func TestDocsGitChangesEndpointRejectsUnsafeLocalConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	// Command-bearing local config does not execute during the status
	// read, but the preview must still refuse it so the UI's publish
	// signal matches publish-time behavior.
	gitfixture.Run(t, repo.Dir, "config", "gpg.program", "/tmp/evil")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/docs/folders/f/git/changes", nil)

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeBadRequest, problem.Code)
	assert.Equal("unsafeGitConfig", problem.Details["reason"])
}

func TestDocsGitReadEndpointsRejectNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	srv := setupDocsGitRouteServer(t, repo.Dir)
	for _, path := range []string{
		"/api/v1/docs/folders/f/git",
		"/api/v1/docs/folders/f/git/changes",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = "203.0.113.7:54321"
		rr := httptest.NewRecorder()

		srv.ServeHTTP(rr, req)

		require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
		var problem httpapi.ProblemError
		require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
		assert.Equal(httpapi.CodeForbidden, problem.Code)
		assert.Equal("loopbackOnly", problem.Details["reason"])
	}
}

func TestDocsGitPublishEndpointHappyPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: update new.md\n\n- new.md\n"),
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.PublishResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.NotEmpty(body.Commit)
	assert.Equal(body.Commit[:7], body.ShortCommit)
	assert.Equal("main", body.Branch)
	assert.Equal("origin/main", body.Upstream)
	assert.True(body.Pushed)
	require.Len(body.Files, 1)
	assert.Equal("new.md", body.Files[0].Path)
}

func TestDocsGitPublishEndpointAcceptsLargeMessageBelowRouteLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: " + strings.Repeat("large ", 2<<18)),
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.PublishResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.NotEmpty(body.Commit)
	assert.Equal(body.Commit, strings.TrimSpace(string(gitfixture.Run(t, repo.Remote, "rev-parse", "main"))))
}

func TestDocsGitPublishEndpointPushesConfiguredUpstreamDespitePushDefaults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	backup := t.TempDir()
	gitfixture.Run(t, backup, "init", "--bare")
	gitfixture.Run(t, repo.Dir, "remote", "add", "backup", backup)
	gitfixture.Run(t, repo.Dir, "push", "backup", "main:main")
	backupInitial := strings.TrimSpace(string(gitfixture.Run(t, backup, "rev-parse", "main")))
	gitfixture.Run(t, repo.Dir, "config", "remote.pushDefault", "backup")
	gitfixture.Run(t, repo.Dir, "config", "push.default", "current")
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: explicit upstream"),
	})

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.PublishResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal("origin/main", body.Upstream)
	assert.True(body.Pushed)
	assert.Equal(body.Commit, strings.TrimSpace(string(gitfixture.Run(t, repo.Remote, "rev-parse", "main"))))
	assert.Equal(backupInitial, strings.TrimSpace(string(gitfixture.Run(t, backup, "rev-parse", "main"))))
}

func TestDocsGitPublishEndpointRejectsNonLoopback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	srv := setupDocsGitRouteServer(t, repo.Dir)
	body, err := json.Marshal(generated.PublishDocsGitJSONRequestBody{Message: new("docs: x")})
	require.NoError(err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/docs/folders/f/git/publish", bytes.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusForbidden, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal(httpapi.CodeForbidden, problem.Code)
	assert.Equal("loopbackOnly", problem.Details["reason"])
}

func TestDocsGitPublishEndpointRejectsNonJSONContentType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupDocsGitRouteServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/docs/folders/f/git/publish", strings.NewReader("docs: x"))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	resp := rr.Result()
	defer resp.Body.Close()
	require.Equal(http.StatusUnsupportedMediaType, resp.StatusCode)
	assert.Equal("application/problem+json", resp.Header.Get("Content-Type"))

	var body struct {
		Status int `json:"status"`
	}
	require.NoError(json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(http.StatusUnsupportedMediaType, body.Status)
}

func TestDocsGitPublishEndpointErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	emptyRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("   \n\t"),
	})

	require.Equal(http.StatusBadRequest, emptyRR.Code, emptyRR.Body.String())
	var empty httpapi.ProblemError
	require.NoError(json.NewDecoder(emptyRR.Body).Decode(&empty))
	assert.Equal("emptyMessage", empty.Details["reason"])

	missingRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/missing/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: x"),
	})

	assert.Equal(http.StatusNotFound, missingRR.Code, missingRR.Body.String())
}

func TestDocsGitPublishEndpointNoUpstreamAndCommitFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	noUpstream := gitfixture.NewRepository(t, false)
	noUpstream.Write(t, "new.md", "# new\n")
	noUpstreamSrv := setupDocsGitRouteServer(t, noUpstream.Dir)

	noUpstreamRR := testutil.DoJSON(t, noUpstreamSrv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: x"),
	})

	require.Equal(http.StatusBadRequest, noUpstreamRR.Code, noUpstreamRR.Body.String())
	var noUpstreamProblem httpapi.ProblemError
	require.NoError(json.NewDecoder(noUpstreamRR.Body).Decode(&noUpstreamProblem))
	assert.Equal("noUpstream", noUpstreamProblem.Details["reason"])
	assert.Contains(noUpstreamProblem.Detail, "git push -u origin main")

	// Force the commit failure with a pre-existing ref lock rather than a
	// command-bearing signer (which the publish safety gate would reject):
	// staging the markdown succeeds, then the commit cannot lock the branch
	// ref. This keeps coverage for git stderr passing through to the detail.
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	lockPath := filepath.Join(repo.Dir, ".git", "refs", "heads", "main.lock")
	require.NoError(os.WriteFile(lockPath, nil, 0o644))
	commitFailSrv := setupDocsGitRouteServer(t, repo.Dir)

	commitFailRR := testutil.DoJSON(t, commitFailSrv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: x"),
	})

	require.Equal(http.StatusInternalServerError, commitFailRR.Code, commitFailRR.Body.String())
	var commitFailProblem httpapi.ProblemError
	require.NoError(json.NewDecoder(commitFailRR.Body).Decode(&commitFailProblem))
	assert.Equal("commitFailed", commitFailProblem.Details["reason"])
	assert.Contains(strings.ToLower(commitFailProblem.Detail), "lock")
	assert.NotContains(commitFailProblem.Detail, "exit status")
}

func TestDocsGitPublishEndpointRejectsUnsafeGitConfig(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	gitfixture.Run(t, repo.Dir, "config", "filter.evil.clean", "/bin/sh -c evil")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: x"),
	})

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("unsafeGitConfig", problem.Details["reason"])
}

func TestDocsGitPublishEndpointIgnoresDocsRepoHooks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "new.md", "# new\n")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hookDir := filepath.Join(repo.Dir, ".git", "hooks")
	require.NoError(os.MkdirAll(hookDir, 0o755))
	hook := "#!/bin/sh\necho hooked > \"" + marker + "\"\nexit 1\n"
	require.NoError(os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hook), 0o755))
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: x"),
	})

	assert.Equal(http.StatusOK, rr.Code, rr.Body.String())
	assert.NoFileExists(marker, "docs repo hook executed during publish")
}

func TestDocsGitPublishEndpointProblemMappings(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T) *server.Server
		wantStatus int
		wantReason string
	}{
		{
			name: "no markdown changes",
			setup: func(t *testing.T) *server.Server {
				repo := gitfixture.NewRepository(t, true)
				repo.Write(t, "code.go", "package x\n")
				return setupDocsGitRouteServer(t, repo.Dir)
			},
			wantStatus: http.StatusBadRequest,
			wantReason: "noMarkdownChanges",
		},
		{
			name: "not a git repo",
			setup: func(t *testing.T) *server.Server {
				return setupDocsGitRouteServer(t, t.TempDir())
			},
			wantStatus: http.StatusBadRequest,
			wantReason: "notGitRepo",
		},
		{
			name: "index not clean",
			setup: func(t *testing.T) *server.Server {
				repo := gitfixture.NewRepository(t, true)
				repo.Write(t, "new.md", "# new\n")
				repo.Write(t, "code.go", "package x\n")
				gitfixture.Run(t, repo.Dir, "add", "code.go")
				return setupDocsGitRouteServer(t, repo.Dir)
			},
			wantStatus: http.StatusConflict,
			wantReason: "indexNotClean",
		},
		{
			name: "conflict",
			setup: func(t *testing.T) *server.Server {
				repo := gitfixture.NewRepository(t, true)
				gitfixture.Run(t, repo.Dir, "checkout", "-b", "side")
				repo.Write(t, "seed.md", "side version\n")
				gitfixture.Run(t, repo.Dir, "commit", "-am", "side")
				gitfixture.Run(t, repo.Dir, "checkout", "main")
				repo.Write(t, "seed.md", "main version\n")
				gitfixture.Run(t, repo.Dir, "commit", "-am", "main")
				out, _, mergeErr := gitsafe.Runner().Run(
					t.Context(), repo.Dir, nil, "merge", "side",
				)
				require.Error(t, mergeErr, "expected merge conflict, got clean merge: %s", out)
				return setupDocsGitRouteServer(t, repo.Dir)
			},
			wantStatus: http.StatusConflict,
			wantReason: "conflict",
		},
		{
			name: "push target inside docs folder",
			setup: func(t *testing.T) *server.Server {
				repo := gitfixture.NewRepository(t, true)
				repo.Write(t, "new.md", "# new\n")
				gitfixture.Run(t, repo.Dir, "init", "--bare", "evil.git")
				gitfixture.Run(t, repo.Dir, "remote", "set-url", "origin", "./evil.git")
				return setupDocsGitRouteServer(t, repo.Dir)
			},
			wantStatus: http.StatusBadRequest,
			wantReason: "unsafeGitConfig",
		},
		{
			name: "push failed after commit",
			setup: func(t *testing.T) *server.Server {
				repo := gitfixture.NewRepository(t, true)
				repo.Write(t, "new.md", "# new\n")
				gitfixture.Run(t, repo.Dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing-origin"))
				return setupDocsGitRouteServer(t, repo.Dir)
			},
			wantStatus: http.StatusBadGateway,
			wantReason: "pushFailedAfterCommit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := tc.setup(t)

			rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
				Message: new("docs: x"),
			})

			require.Equal(tc.wantStatus, rr.Code, rr.Body.String())
			var problem httpapi.ProblemError
			require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
			assert.Equal(tc.wantReason, problem.Details["reason"])
			if tc.wantReason == "pushFailedAfterCommit" {
				assert.NotEmpty(problem.Details["commit"])
				assert.NotContains(problem.Detail, "exit status")
			}
		})
	}
}

func TestDocsGitPublishEndpointRejectsConcurrentInFlightPublish(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.Write(t, "blocked.md", "# blocked\n")
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: 8091,
		DocFolders: []config.DocFolder{
			{ID: "f", Name: "F", Path: repo.Dir},
			{ID: "alias", Name: "Alias", Path: repo.Dir},
		},
	}
	registry := docs.NewRegistry(cfg.DocFolders, docs.WithGitRunner(gitsafe.Runner()))
	srv := servertest.New(t, dbtest.Open(t), nil, nil, "/", cfg, server.ServerOptions{
		DocsRegistry:                  registry,
		HostCheckAllowLoopbackAnyPort: true,
	})

	// The publish safety gate forbids command-bearing config, so hold the
	// publish in-flight by hanging its push: point origin at an HTTP server
	// that blocks the initial ref advertisement until released. The commit
	// succeeds first, then push parks here while the single-flight lock is
	// held, letting a concurrent request observe the 409.
	gotPush := make(chan struct{})
	release := make(chan struct{})
	var gotPushOnce, releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			gotPushOnce.Do(func() { close(gotPush) })
			<-release
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hung.Close()
	defer doRelease()
	gitfixture.Run(t, repo.Dir, "remote", "set-url", "origin", hung.URL+"/repo.git")

	publishAsync := func(message string) <-chan *httptest.ResponseRecorder {
		body, err := json.Marshal(generated.PublishDocsGitJSONRequestBody{Message: new(message)})
		require.NoError(err)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/docs/folders/f/git/publish",
				bytes.NewReader(body),
			)
			req.Host = "127.0.0.1"
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			done <- rr
		}()
		return done
	}

	firstDone := publishAsync("docs: blocked")
	firstConsumed := false
	defer func() {
		doRelease()
		if !firstConsumed {
			select {
			case <-firstDone:
			case <-time.After(5 * time.Second):
				require.Fail("timed out waiting for first publish cleanup")
			}
		}
	}()

	select {
	case <-gotPush:
	case <-time.After(5 * time.Second):
		require.FailNow("timed out waiting for first publish to reach push")
	}

	secondDone := publishAsync("docs: concurrent")
	var conflictRR *httptest.ResponseRecorder
	select {
	case conflictRR = <-secondDone:
	case <-time.After(2 * time.Second):
		require.FailNow("timed out waiting for concurrent publish conflict")
	}

	require.Equal(http.StatusConflict, conflictRR.Code, conflictRR.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(conflictRR.Body).Decode(&problem))
	assert.Equal(httpapi.CodeConflict, problem.Code)
	assert.Equal("publishInProgress", problem.Details["reason"])

	aliasPull := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/alias/git/pull", nil)
	require.Equal(http.StatusConflict, aliasPull.Code, aliasPull.Body.String())
	var aliasProblem httpapi.ProblemError
	require.NoError(json.NewDecoder(aliasPull.Body).Decode(&aliasProblem))
	assert.Equal(httpapi.CodeConflict, aliasProblem.Code)
	assert.Equal("gitOperationInProgress", aliasProblem.Details["reason"])

	// Release the hung push; the first publish commits but fails to push.
	doRelease()
	var firstRR *httptest.ResponseRecorder
	select {
	case firstRR = <-firstDone:
		firstConsumed = true
	case <-time.After(5 * time.Second):
		require.Fail("timed out waiting for first publish")
	}
	require.Equal(http.StatusBadGateway, firstRR.Code, firstRR.Body.String())
	var firstProblem httpapi.ProblemError
	require.NoError(json.NewDecoder(firstRR.Body).Decode(&firstProblem))
	assert.Equal("pushFailedAfterCommit", firstProblem.Details["reason"])

	// With the lock released and a working remote, a fresh publish succeeds.
	gitfixture.Run(t, repo.Dir, "remote", "set-url", "origin", repo.Remote)
	repo.Write(t, "after.md", "# after\n")
	afterRR := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/publish", generated.PublishDocsGitJSONRequestBody{
		Message: new("docs: after"),
	})

	assert.Equal(http.StatusOK, afterRR.Code, afterRR.Body.String())
}

func TestDocsGitPullEndpointFastForwards(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	want := repo.RemoteCommit(t, "remote.md", "# remote\n")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/pull", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.PullResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.False(body.UpToDate)
	assert.Equal(want, body.Commit)
	assert.Equal("main", body.Branch)
	assert.Equal("origin/main", body.Upstream)
	_, statErr := os.Stat(filepath.Join(repo.Dir, "remote.md"))
	assert.NoError(statErr)
}

func TestDocsGitPullEndpointUpToDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/pull", nil)

	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body docs.PullResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	assert.True(body.UpToDate)
	assert.NotEmpty(body.Commit)
}

func TestDocsGitPullEndpointDivergedIs409(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, true)
	repo.RemoteCommit(t, "remote.md", "remote\n")
	repo.Write(t, "local.md", "local\n")
	gitfixture.Run(t, repo.Dir, "add", "--", "local.md")
	gitfixture.Run(t, repo.Dir, "commit", "-m", "local update")
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/pull", nil)

	require.Equal(http.StatusConflict, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("diverged", problem.Details["reason"])
}

func TestDocsGitPullEndpointNoUpstreamIs400(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	repo := gitfixture.NewRepository(t, false)
	srv := setupDocsGitRouteServer(t, repo.Dir)

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/docs/folders/f/git/pull", nil)

	require.Equal(http.StatusBadRequest, rr.Code, rr.Body.String())
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("noUpstream", problem.Details["reason"])
	assert.Contains(problem.Details["suggested_command"], "--set-upstream-to")
}
