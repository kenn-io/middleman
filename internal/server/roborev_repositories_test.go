package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/projects"
	"go.kenn.io/forge/internal/testutil"
)

func TestRoborevRepositoryProbeCachesDefinitiveResultsAndDeduplicatesIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var inventoryCalls atomic.Int32
	var hookPathCalls atomic.Int32
	var inspectCalls atomic.Int32
	probe := newRoborevRepositoryProbeWithDeps(
		[]projects.KnownPlatformHost{{Platform: "github", Host: "github.com"}},
		roborevRepositoryProbeDeps{
			now: time.Now,
			loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
				inventoryCalls.Add(1)
				return []roborevTrackedRepository{
					{RootPath: "/checkout/main", Identity: "https://github.com/acme/widgets.git"},
					{RootPath: "/checkout/worktree", Identity: "git@github.com:acme/widgets.git"},
				}, nil
			},
			resolveHookPath: func(_ context.Context, root string) (string, error) {
				hookPathCalls.Add(1)
				return "/shared/hooks/post-commit", nil
			},
			inspectHook: func(string) (bool, error) {
				inspectCalls.Add(1)
				return true, nil
			},
		},
	)

	first, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	second, err := probe.configuredRepositories(t.Context())
	require.NoError(err)

	require.Len(first, 1)
	assert.Equal(roborevConfiguredRepositoryResponse{
		Provider:     "github",
		PlatformHost: "github.com",
		RepoPath:     "acme/widgets",
		Owner:        "acme",
		Name:         "widgets",
	}, first[0])
	assert.Equal(first, second)
	assert.Equal(int32(1), inventoryCalls.Load())
	assert.Equal(int32(2), hookPathCalls.Load())
	assert.Equal(int32(1), inspectCalls.Load())
}

func TestRoborevRepositoryProbeInvalidateReloadsInventoryAndDefinitiveResults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var calls atomic.Int32
	probe := newRoborevRepositoryProbeWithDeps(
		[]projects.KnownPlatformHost{{Platform: "github", Host: "github.com"}},
		roborevRepositoryProbeDeps{
			now: time.Now,
			loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
				if calls.Add(1) == 1 {
					return []roborevTrackedRepository{{
						RootPath: "/first", Identity: "https://github.com/acme/first.git",
					}}, nil
				}
				return []roborevTrackedRepository{{
					RootPath: "/second", Identity: "https://github.com/acme/second.git",
				}}, nil
			},
			resolveHookPath: func(_ context.Context, root string) (string, error) {
				return root + "/post-commit", nil
			},
			inspectHook: func(string) (bool, error) { return true, nil },
		},
	)

	first, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal("acme/first", first[0].RepoPath)

	probe.Invalidate()
	second, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal("acme/second", second[0].RepoPath)
	assert.Equal(int32(2), calls.Load())
}

func TestRoborevRepositoryProbeInvalidateFencesInFlightRefresh(t *testing.T) {
	require := require.New(t)
	started := make(chan struct{})
	freshStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	probe := newRoborevRepositoryProbeWithDeps(
		[]projects.KnownPlatformHost{{Platform: "github", Host: "github.com"}},
		roborevRepositoryProbeDeps{
			now: time.Now,
			loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
					return []roborevTrackedRepository{{
						RootPath: "/stale", Identity: "https://github.com/acme/stale.git",
					}}, nil
				}
				close(freshStarted)
				return []roborevTrackedRepository{{
					RootPath: "/fresh", Identity: "https://github.com/acme/fresh.git",
				}}, nil
			},
			resolveHookPath: func(_ context.Context, root string) (string, error) {
				return root + "/post-commit", nil
			},
			inspectHook: func(string) (bool, error) { return true, nil },
		},
	)

	result := make(chan []roborevConfiguredRepositoryResponse, 1)
	go func() {
		configured, _ := probe.configuredRepositories(t.Context())
		result <- configured
	}()
	<-started
	probe.Invalidate()
	select {
	case <-freshStarted:
	case <-time.After(time.Second):
		require.Fail("invalidation did not start a fresh probe")
	}
	configured := <-result
	releaseOnce.Do(func() { close(release) })
	require.Len(configured, 1)
	require.Equal("acme/fresh", configured[0].RepoPath)
	require.Equal(int32(2), calls.Load())
}

func TestRoborevRepositoryProbeCoalescesConcurrentRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	started := make(chan struct{})
	release := make(chan struct{})
	waiterJoined := make(chan struct{})
	var calls atomic.Int32
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	probe := newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: time.Now,
		onWaitForInFlight: func() {
			close(waiterJoined)
		},
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return []roborevTrackedRepository{}, nil
		},
		resolveHookPath: func(context.Context, string) (string, error) { return "", nil },
		inspectHook:     func(string) (bool, error) { return false, nil },
	})

	results := make(chan error, 2)
	go func() {
		_, err := probe.configuredRepositories(t.Context())
		results <- err
	}()
	<-started
	go func() {
		_, err := probe.configuredRepositories(t.Context())
		results <- err
	}()
	select {
	case <-waiterJoined:
	case <-time.After(time.Second):
		require.Fail("second request did not join the in-flight probe")
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(<-results)
	require.NoError(<-results)
	assert.Equal(int32(1), calls.Load())
}

func TestRoborevRepositoryProbeCallerCancellationDoesNotPoisonWaiters(t *testing.T) {
	require := require.New(t)
	started := make(chan struct{})
	release := make(chan struct{})
	waiterJoined := make(chan struct{})
	probe := newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now:               time.Now,
		onWaitForInFlight: func() { close(waiterJoined) },
		loadInventory: func(ctx context.Context) ([]roborevTrackedRepository, error) {
			close(started)
			select {
			case <-release:
				return []roborevTrackedRepository{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		resolveHookPath: func(context.Context, string) (string, error) { return "", nil },
		inspectHook:     func(string) (bool, error) { return false, nil },
	})

	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := probe.configuredRepositories(leaderCtx)
		leader <- err
	}()
	<-started
	waiter := make(chan error, 1)
	go func() {
		_, err := probe.configuredRepositories(t.Context())
		waiter <- err
	}()
	<-waiterJoined
	cancelLeader()
	require.ErrorIs(<-leader, context.Canceled)
	close(release)
	require.NoError(<-waiter)
}

func TestRoborevRepositoryProbeBoundsHookResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var active atomic.Int32
	var maximum atomic.Int32
	repositories := make([]roborevTrackedRepository, 12)
	for i := range repositories {
		repositories[i] = roborevTrackedRepository{
			RootPath: fmt.Sprintf("/checkout/%d", i),
			Identity: fmt.Sprintf("https://github.com/acme/repo-%d.git", i),
		}
	}
	probe := newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now:           time.Now,
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) { return repositories, nil },
		resolveHookPath: func(_ context.Context, root string) (string, error) {
			current := active.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			return root + "/post-commit", nil
		},
		inspectHook: func(string) (bool, error) { return true, nil },
	})

	configured, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	assert.Len(configured, 12)
	assert.Greater(maximum.Load(), int32(1))
	assert.LessOrEqual(maximum.Load(), int32(roborevHookProbeWorkers))
}

func TestRoborevRepositoryProbeRetriesTransientCheckoutFailureAfterCooldown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	var failingCalls atomic.Int32
	probe := newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: func() time.Time { return now },
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			return []roborevTrackedRepository{
				{RootPath: "/positive", Identity: "https://github.com/acme/widgets.git"},
				{RootPath: "/transient", Identity: "https://github.com/acme/tools.git"},
			}, nil
		},
		resolveHookPath: func(_ context.Context, root string) (string, error) {
			if root == "/transient" && failingCalls.Add(1) == 1 {
				return "", errors.New("temporary git failure")
			}
			return root + "/post-commit", nil
		},
		inspectHook: func(string) (bool, error) { return true, nil },
	})

	first, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	require.Len(first, 1)
	assert.Equal("acme/widgets", first[0].RepoPath)
	now = now.Add(29 * time.Second)
	second, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	assert.Len(second, 1)
	assert.Equal(int32(1), failingCalls.Load())
	now = now.Add(time.Second)
	third, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	assert.Len(third, 2)
	assert.Equal(int32(2), failingCalls.Load())
}

func TestRoborevRepositoryProbeStartsCheckoutCooldownWhenFailureCompletes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	var calls atomic.Int32
	probe := newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() },
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			return []roborevTrackedRepository{
				{RootPath: "/slow", Identity: "https://github.com/acme/widgets.git"},
			}, nil
		},
		resolveHookPath: func(context.Context, string) (string, error) {
			if calls.Add(1) == 1 {
				nowNanos.Store(start.Add(roborevProbeRetryCooldown + time.Second).UnixNano())
				return "", errors.New("slow git failure")
			}
			return "/hooks/post-commit", nil
		},
		inspectHook: func(string) (bool, error) { return true, nil },
	})

	first, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	assert.Empty(first)
	assert.Equal(int32(1), calls.Load())

	nowNanos.Store(start.Add(2*roborevProbeRetryCooldown + time.Second).UnixNano())
	second, err := probe.configuredRepositories(t.Context())
	require.NoError(err)
	assert.Len(second, 1)
	assert.Equal(int32(2), calls.Load())
}

func TestInspectRoborevPostCommitHook(t *testing.T) {
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    bool
	}{
		{name: "generated marker", content: "#!/bin/sh\n# roborev post-commit hook v4\n", mode: 0o755, want: true},
		{name: "current variable command", content: "#!/bin/sh\n\"$ROBOREV\" post-commit\n", mode: 0o755, want: true},
		{name: "current direct command", content: "#!/bin/sh\nroborev post-commit\n", mode: 0o755, want: true},
		{name: "legacy variable command", content: "#!/bin/sh\n\"$ROBOREV\" enqueue --quiet\n", mode: 0o755, want: true},
		{name: "legacy direct command", content: "#!/bin/sh\nroborev enqueue --quiet\n", mode: 0o755, want: true},
		{name: "unrelated executable", content: "#!/bin/sh\necho hello\n", mode: 0o755, want: false},
		{name: "partial post commit command", content: "#!/bin/sh\nroborev post-commit-disabled\n", mode: 0o755, want: false},
		{name: "partial enqueue command", content: "#!/bin/sh\nroborev enqueue-old\n", mode: 0o755, want: false},
		{name: "commented command", content: "#!/bin/sh\n# roborev post-commit\n", mode: 0o755, want: false},
		{name: "command in string", content: "#!/bin/sh\necho 'roborev post-commit'\n", mode: 0o755, want: false},
		{name: "non executable", content: "#!/bin/sh\nroborev post-commit\n", mode: 0o644, want: runtime.GOOS == "windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "post-commit")
			require.NoError(os.WriteFile(path, []byte(tt.content), tt.mode))
			got, err := inspectRoborevPostCommitHook(path)
			require.NoError(err)
			assert.Equal(tt.want, got)
		})
	}

	assert := assert.New(t)
	require := require.New(t)
	missing, err := inspectRoborevPostCommitHook(filepath.Join(t.TempDir(), "missing"))
	require.NoError(err)
	assert.False(missing)
	directory, err := inspectRoborevPostCommitHook(t.TempDir())
	require.NoError(err)
	assert.False(directory)
}

func TestLoadRoborevRepositoryInventoryValidatesCompleteEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []roborevTrackedRepository
	}{
		{
			name: "valid repo without review jobs",
			body: `{"repos":[{"root_path":"/repo","identity":"https://github.com/acme/widgets.git"}],"total_count":0}`,
			want: []roborevTrackedRepository{{RootPath: "/repo", Identity: "https://github.com/acme/widgets.git"}},
		},
		{
			name: "valid mixed identity availability",
			body: `{"repos":[{"root_path":"/repo","identity":"https://github.com/acme/widgets.git"},{"root_path":"/local"}],"total_count":3}`,
			want: []roborevTrackedRepository{
				{RootPath: "/repo", Identity: "https://github.com/acme/widgets.git"},
				{RootPath: "/local"},
			},
		},
		{name: "valid null repos", body: `{"repos":null,"total_count":0}`, want: []roborevTrackedRepository{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			got, err := loadRoborevRepositoryInventory(server.Client(), server.URL)(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	invalid := []string{
		`{}`,
		`{"repos":[{"identity":"https://github.com/acme/widgets.git"}],"total_count":1}`,
		`{"repos":[],"total_count":0} trailing`,
	}
	for _, body := range invalid {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		_, err := loadRoborevRepositoryInventory(server.Client(), server.URL)(t.Context())
		server.Close()
		require.Error(t, err, body)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", roborevInventoryMaxBytes+1))
	}))
	defer server.Close()
	_, err := loadRoborevRepositoryInventory(server.Client(), server.URL)(t.Context())
	assert.Error(t, err, "oversized inventory must be rejected")
}

func TestListRoborevConfiguredRepositories(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupTestServerWithRoborev(t, "http://127.0.0.1:1")
	srv.roborevRepositories = newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: time.Now,
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			return []roborevTrackedRepository{
				{RootPath: "/checkout/widgets", Identity: "https://github.com/acme/widgets.git"},
			}, nil
		},
		resolveHookPath: func(context.Context, string) (string, error) { return "/hooks/post-commit", nil },
		inspectHook:     func(string) (bool, error) { return true, nil },
	})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/roborev/configured-repositories", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body struct {
		Repositories []roborevConfiguredRepositoryResponse `json:"repositories"`
		Complete     bool                                  `json:"complete"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.Len(body.Repositories, 1)
	assert.True(body.Complete)
	assert.Equal("github", body.Repositories[0].Provider)
	assert.Equal("github.com", body.Repositories[0].PlatformHost)
	assert.Equal("acme/widgets", body.Repositories[0].RepoPath)
	assert.NotContains(rr.Body.String(), "/checkout/widgets")
}

func TestListRoborevConfiguredRepositoriesMarksPartialResultsIncomplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupTestServerWithRoborev(t, "http://127.0.0.1:1")
	srv.roborevRepositories = newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: time.Now,
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			return []roborevTrackedRepository{
				{RootPath: "/configured", Identity: "https://github.com/acme/widgets.git"},
				{RootPath: "/unresolved", Identity: "https://github.com/acme/tools.git"},
			}, nil
		},
		resolveHookPath: func(_ context.Context, root string) (string, error) {
			if root == "/unresolved" {
				return "", errors.New("temporary git failure")
			}
			return "/hooks/post-commit", nil
		},
		inspectHook: func(string) (bool, error) { return true, nil },
	})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/roborev/configured-repositories", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())
	var body roborevConfiguredRepositoriesResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&body))
	require.Len(body.Repositories, 1)
	assert.Equal("acme/widgets", body.Repositories[0].RepoPath)
	assert.False(body.Complete)
}

func TestListRoborevConfiguredRepositoriesReturnsTypedUnavailableWithoutBlockingSummaries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := setupTestServerWithRoborev(t, "http://private.invalid:7373")
	srv.roborevRepositories = newRoborevRepositoryProbeWithDeps(nil, roborevRepositoryProbeDeps{
		now: time.Now,
		loadInventory: func(context.Context) ([]roborevTrackedRepository, error) {
			return nil, errors.New("private daemon /private/checkout")
		},
	})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/roborev/configured-repositories", nil)
	require.Equal(http.StatusServiceUnavailable, rr.Code)
	assert.Equal("application/problem+json", rr.Header().Get("Content-Type"))
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	require.NoError(json.NewDecoder(rr.Body).Decode(&problem))
	assert.Equal("serviceUnavailable", problem.Code)
	assert.Equal("roborev repository configuration unavailable", problem.Detail)
	assert.NotContains(rr.Body.String(), "private.invalid")
	assert.NotContains(rr.Body.String(), "/private/checkout")

	summaries := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repos/summary", nil)
	assert.Equal(http.StatusOK, summaries.Code)
}
