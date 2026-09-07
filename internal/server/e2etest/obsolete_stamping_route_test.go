package e2etest

import (
	"context"
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
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/forgejo"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func TestForgejoSyncRouteStampsObsoleteCommitEventsAcrossForcePushes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	require.NoError(os.MkdirAll(work, 0o755))
	runGit := func(dir string, args ...string) string {
		t.Helper()
		out, stderr, err := gitcmd.New().Run(ctx, dir, nil, args...)
		require.NoError(err, "git %v: %s%s", args, out, stderr)
		return strings.TrimSpace(string(out))
	}
	commit := func(path, contents, message string) string {
		t.Helper()
		require.NoError(os.WriteFile(filepath.Join(work, path), []byte(contents), 0o644))
		runGit(work, "add", path)
		runGit(work, "commit", "-m", message)
		return runGit(work, "rev-parse", "HEAD")
	}

	runGit(work, "init", "-b", "main")
	runGit(work, "config", "user.email", "fixture@example.invalid")
	runGit(work, "config", "user.name", "Fixture")
	base := commit("base.txt", "m1\n", "base m1")
	runGit(work, "checkout", "-b", "feature")
	a1 := commit("lineage-a.txt", "a1\n", "lineage a1")
	a2 := commit("lineage-a.txt", "a2\n", "lineage a2")
	a3 := commit("lineage-a.txt", "a3\n", "lineage a3")
	runGit(work, "checkout", "-b", "feature-b", base)
	b1 := commit("lineage-b.txt", "b1\n", "lineage b1")
	b2 := commit("lineage-b.txt", "b2\n", "lineage b2")
	origin := filepath.Join(root, "origin.git")
	runGit(root, "clone", "--bare", work, origin)
	runGit(origin, "update-ref", "refs/heads/feature", a3)

	type providerState struct {
		head      string
		prState   string
		commits   []string
		updatedAt time.Time
	}
	var stateMu sync.RWMutex
	state := providerState{
		head:      a3,
		prState:   "open",
		commits:   []string{a1, a2, a3},
		updatedAt: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
	}
	setProviderState := func(head, prState string, commits []string) {
		t.Helper()
		stateMu.Lock()
		state.head = head
		state.prState = prState
		state.commits = append([]string(nil), commits...)
		state.updatedAt = state.updatedAt.Add(time.Minute)
		stateMu.Unlock()
	}
	writePullRequest := func(w http.ResponseWriter, current providerState) {
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id": 101, "number": 1, "title": "Synthetic merge request", "state": current.prState,
			"url":      "https://codeberg.org/api/v1/repos/owner/repo/pulls/1",
			"html_url": "https://codeberg.org/owner/repo/pulls/1",
			"user":     map[string]any{"id": 3, "login": "developer", "full_name": "Developer"},
			"head": map[string]any{
				"ref": "feature", "sha": current.head,
				"repo": map[string]any{"id": 1, "name": "repo", "full_name": "owner/repo", "clone_url": origin},
			},
			"base":       map[string]any{"ref": "main", "sha": base},
			"created_at": "2026-08-05T11:00:00Z",
			"updated_at": current.updatedAt.Format(time.RFC3339),
		}))
	}

	providerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stateMu.RLock()
		current := state
		current.commits = append([]string(nil), state.commits...)
		stateMu.RUnlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "name": "repo", "full_name": "owner/repo",
				"html_url": "https://codeberg.org/owner/repo", "clone_url": origin,
				"default_branch": "main", "has_issues": true, "has_pull_requests": true,
				"owner": map[string]any{"id": 2, "login": "owner"},
			}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/pulls/1":
			writePullRequest(w, current)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/owner/repo/pulls/1":
			// UI state mutation: flip the provider's PR state the way the
			// real provider does and return the updated pull request; the
			// handler commits this response as the transition snapshot.
			var body struct {
				State string `json:"state"`
			}
			assert.NoError(json.NewDecoder(r.Body).Decode(&body))
			stateMu.Lock()
			if body.State != "" {
				state.prState = body.State
			}
			state.updatedAt = state.updatedAt.Add(time.Minute)
			current = state
			current.commits = append([]string(nil), state.commits...)
			stateMu.Unlock()
			writePullRequest(w, current)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/issues/1/comments":
			assert.NoError(json.NewEncoder(w).Encode([]any{}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/pulls/1/reviews":
			assert.NoError(json.NewEncoder(w).Encode([]any{}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/pulls/1/commits":
			commits := make([]map[string]any, 0, len(current.commits))
			for i, sha := range current.commits {
				commits = append(commits, map[string]any{
					"sha": sha, "created": current.updatedAt.Add(time.Duration(i-len(current.commits)) * time.Minute),
					"html_url": "https://codeberg.org/owner/repo/commit/" + sha,
					"commit": map[string]any{
						"message": "synthetic commit " + sha[:8],
						"author":  map[string]any{"name": "Developer", "date": current.updatedAt.Format(time.RFC3339)},
					},
				})
			}
			assert.NoError(json.NewEncoder(w).Encode(commits))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/repos/owner/repo/commits/") && strings.HasSuffix(r.URL.Path, "/statuses"):
			assert.NoError(json.NewEncoder(w).Encode([]any{}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/actions/runs":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"total_count": 0, "workflow_runs": []any{},
			}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(providerAPI.Close)

	provider, err := forgejo.NewClient(
		platform.DefaultForgejoHost,
		staticTokenSource("token"),
		forgejo.WithBaseURLForTesting(providerAPI.URL), forgejo.WithTransport(http.DefaultTransport))
	require.NoError(err)
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	database := dbtest.Open(t)
	_, err = database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       string(platform.KindForgejo),
		PlatformHost:   platform.DefaultForgejoHost,
		PlatformRepoID: "1",
		Owner:          "owner",
		Name:           "repo",
		RepoPath:       "owner/repo",
	})
	require.NoError(err)
	clones := gitclone.New(t.TempDir(), nil)
	repo := ghclient.RepoRef{
		Platform:           platform.KindForgejo,
		PlatformHost:       platform.DefaultForgejoHost,
		PlatformExternalID: "1",
		Owner:              "owner",
		Name:               "repo",
		RepoPath:           "owner/repo",
		CloneURL:           origin,
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, clones, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(srv.Shutdown(shutdownCtx))
	})

	syncPath := "/api/v1/pulls/forgejo/owner/repo/1/sync"
	detailPath := "/api/v1/pulls/forgejo/owner/repo/1"
	syncResponse := doJSONRequest(t, srv, http.MethodPost, syncPath, map[string]any{})
	require.Equal(http.StatusOK, syncResponse.Code, syncResponse.Body.String())

	assertDetailFlags := func(want map[string]bool) {
		t.Helper()
		detailResponse := doJSONRequest(t, srv, http.MethodGet, detailPath, nil)
		require.Equal(http.StatusOK, detailResponse.Code, detailResponse.Body.String())
		var detail struct {
			Events []struct {
				PlatformExternalID string `json:"PlatformExternalID"`
				EventType          string `json:"EventType"`
				MetadataJSON       string `json:"MetadataJSON"`
			} `json:"events"`
		}
		require.NoError(json.Unmarshal(detailResponse.Body.Bytes(), &detail))
		metadataBySHA := make(map[string]map[string]any)
		for _, event := range detail.Events {
			if event.EventType != "commit" {
				continue
			}
			var metadata map[string]any
			require.NoError(json.Unmarshal([]byte(event.MetadataJSON), &metadata))
			metadataBySHA[event.PlatformExternalID] = metadata
		}
		for sha, obsolete := range want {
			metadata, ok := metadataBySHA[sha]
			require.True(ok, "detail response missing commit %s", sha)
			assert.Contains(metadata, "commit_order_key", "commit %s lost ordering metadata", sha)
			value, present := metadata["obsolete"]
			if obsolete {
				assert.True(present, "commit %s has no obsolete flag", sha)
				assert.Equal(true, value, "commit %s has the wrong obsolete value", sha)
			} else {
				assert.False(present, "commit %s unexpectedly has an obsolete flag", sha)
			}
		}
	}

	runGit(origin, "update-ref", "refs/heads/feature", b2)
	setProviderState(b2, "open", []string{b1, b2})
	syncResponse = doJSONRequest(t, srv, http.MethodPost, syncPath, map[string]any{})
	require.Equal(http.StatusOK, syncResponse.Code, syncResponse.Body.String())
	assertDetailFlags(map[string]bool{
		a1: true, a2: true, a3: true, b1: false, b2: false,
	})

	// Provider activity advances without the head moving: the provider
	// re-lists the same commits with fresh, unflagged metadata. The re-synced
	// rounds must keep re-injecting the verified flags — the collapse state
	// may never flip on a same-head refresh.
	for range 2 {
		setProviderState(b2, "open", []string{b1, b2})
		syncResponse = doJSONRequest(t, srv, http.MethodPost, syncPath, map[string]any{})
		require.Equal(http.StatusOK, syncResponse.Code, syncResponse.Body.String())
		assertDetailFlags(map[string]bool{
			a1: true, a2: true, a3: true, b1: false, b2: false,
		})
	}

	runGit(origin, "update-ref", "refs/heads/feature", a3)
	setProviderState(a3, "open", []string{a1, a2, a3})
	syncResponse = doJSONRequest(t, srv, http.MethodPost, syncPath, map[string]any{})
	require.Equal(http.StatusOK, syncResponse.Code, syncResponse.Body.String())
	assertDetailFlags(map[string]bool{
		a1: false, a2: false, a3: false, b1: true, b2: true,
	})

	// A force push followed immediately by a UI-driven close: the terminal
	// mutation must flow through the same close-detection path the periodic
	// sync uses, so the transition round computes liveness against the
	// final head instead of freezing the pre-push flags behind an eager
	// local state write.
	runGit(origin, "update-ref", "refs/heads/feature", b2)
	setProviderState(b2, "open", []string{b1, b2})
	closeResponse := doJSONRequest(t, srv, http.MethodPost,
		"/api/v1/pulls/forgejo/owner/repo/1/github-state",
		map[string]any{"state": "closed"},
	)
	require.Equal(http.StatusOK, closeResponse.Code, closeResponse.Body.String())
	assertDetailFlags(map[string]bool{
		a1: true, a2: true, a3: true, b1: false, b2: false,
	})

	// A later sync of the now-closed PR must leave the terminal record
	// alone: merged and closed history is never recomputed.
	syncResponse = doJSONRequest(t, srv, http.MethodPost, syncPath, map[string]any{})
	require.Equal(http.StatusOK, syncResponse.Code, syncResponse.Body.String())
	assertDetailFlags(map[string]bool{
		a1: true, a2: true, a3: true, b1: false, b2: false,
	})
}
