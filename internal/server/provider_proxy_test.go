package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/mcpserver"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/issueapi"
	"go.kenn.io/forge/internal/server/pullapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
)

const (
	proxyTestNodeID = "33333333333333333333333333333333"
	proxyTestHubID  = "44444444444444444444444444444444"
)

type providerPlaneClientFunc func(
	context.Context, federationauth.Scope, *http.Request,
) (*http.Response, error)

func (f providerPlaneClientFunc) Do(
	ctx context.Context, scope federationauth.Scope, request *http.Request,
) (*http.Response, error) {
	return f(ctx, scope, request)
}

type failingProviderResponseBody struct {
	data []byte
	err  error
}

func (b *failingProviderResponseBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (*failingProviderResponseBody) Close() error { return nil }

func TestProviderProxyPreservesHubResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("ETag", `"provider-revision-3"`)
		w.Header().Set("Retry-After", "3")
		w.Header().Set("X-Provider-Safe", "yes")
		w.Header().Set("Connection", "X-Provider-Hop")
		w.Header().Set("X-Provider-Hop", "drop-me")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"staleMutation","detail":"refresh first"}`)
	}))
	t.Cleanup(hub.Close)

	spoke := newProviderProxyTestServer(t, hub, hub.Client(), nil)
	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(response.Body.Close()) })
	body, err := io.ReadAll(response.Body)
	require.NoError(err)

	assert.Equal(http.StatusConflict, response.StatusCode)
	assert.Equal("application/problem+json", response.Header.Get("Content-Type"))
	assert.Equal(`"provider-revision-3"`, response.Header.Get("ETag"))
	assert.Equal("3", response.Header.Get("Retry-After"))
	assert.Equal("yes", response.Header.Get("X-Provider-Safe"))
	assert.Empty(response.Header.Get("X-Provider-Hop"))
	assert.JSONEq(`{"code":"staleMutation","detail":"refresh first"}`, string(body))
}

func TestHubUnavailableDoesNotFallBackToLocalProviderHandler(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	unreachable := "https://" + listener.Addr().String()
	require.NoError(listener.Close())

	var localReads atomic.Int64
	spoke := newProviderProxyTestServer(
		t, nil, &http.Client{Timeout: 100 * time.Millisecond},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			localReads.Add(1)
			_, _ = io.WriteString(w, `{"source":"local-provider-table"}`)
		}),
	)
	spoke.Config.Handler.(*providerDispatchTestHandler).hubURL = unreachable

	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	defer response.Body.Close()
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))

	assert.Equal(http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(httpapi.CodeHubUnavailable, problem.Code)
	assert.Zero(localReads.Load())
}

func TestNodeHEADProviderReadUsesHubGETOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	var hubReads atomic.Int64
	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		hubReads.Add(1)
		assert.Equal(http.MethodHead, r.Method)
		w.Header().Set("X-Provider-Source", "hub")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hub.Close)

	var localReads atomic.Int64
	spoke := newProviderProxyTestServer(
		t, hub, hub.Client(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			localReads.Add(1)
			w.Header().Set("X-Provider-Source", "spoke")
		}),
	)
	request, err := http.NewRequest(http.MethodHead, spoke.URL+"/api/v1/pulls", nil)
	require.NoError(err)
	response, err := spoke.Client().Do(request)
	require.NoError(err)
	defer response.Body.Close()

	assert.Equal(http.StatusOK, response.StatusCode)
	assert.Equal("hub", response.Header.Get("X-Provider-Source"))
	assert.Equal(int64(1), hubReads.Load())
	assert.Zero(localReads.Load())
}

func TestNodeMarkdownImageUsesHubProviderReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var hubReads atomic.Int64
	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, request *http.Request,
	) {
		hubReads.Add(1)
		assert.Equal("/api/v1/repo/github/acme/widget/markdown-image", request.URL.Path)
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "hub image")
	}))
	t.Cleanup(hub.Close)

	var localReads atomic.Int64
	spoke := newProviderProxyTestServer(
		t, hub, hub.Client(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			localReads.Add(1)
			http.Error(w, "spoke provider reader unavailable", http.StatusServiceUnavailable)
		}),
	)
	response, err := spoke.Client().Get(
		spoke.URL + "/api/v1/repo/github/acme/widget/markdown-image?source=" +
			url.QueryEscape("https://github.com/acme/widget/raw/main/image.png"),
	)
	require.NoError(err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(err)

	assert.Equal(http.StatusOK, response.StatusCode)
	assert.Equal("image/png", response.Header.Get("Content-Type"))
	assert.Equal("hub image", string(body))
	assert.Equal(int64(1), hubReads.Load())
	assert.Zero(localReads.Load())
}

func TestProviderWriteTransportFailureReportsUnknownMutationOutcome(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var dispatched atomic.Int64
	proxy := newProviderProxy(providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		dispatched.Add(1)
		assert.Equal(federationauth.ScopeProviderWrite, scope)
		assert.Equal(http.MethodPut, request.Method)
		return nil, providerplane.ErrHubUnavailable
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/settings", nil)
	proxy.ServeHTTP(recorder, request, ProviderRouteRule{
		Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite,
	})

	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(recorder.Body).Decode(&problem))
	assert.Equal(http.StatusBadGateway, recorder.Code)
	assert.Equal(httpapi.CodeMutationOutcomeUnknown, problem.Code)
	assert.Equal(int64(1), dispatched.Load())
}

func TestHubWorkflowMutationTransportFailureIsAmbiguous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var dispatched atomic.Int64
	client := providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		dispatched.Add(1)
		assert.Equal(federationauth.ScopeProviderWrite, scope)
		assert.Equal(http.MethodPut, request.Method)
		return nil, providerplane.ErrHubUnavailable
	})
	server := &Server{providerSource: &hubProviderSource{client: client}}

	_, err := server.MCPBackend().SetWorkflowState(
		t.Context(), mcpserver.ItemIdentity{
			Type: "pr", Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget", Number: 42,
		},
		mcpserver.WorkflowUpdate{Status: "reviewing", ExpectedStatus: "new"},
	)
	var backendErr *mcpserver.Error
	require.ErrorAs(err, &backendErr)
	assert.Equal(string(httpapi.CodeMutationOutcomeUnknown), backendErr.Code)
	assert.True(backendErr.Ambiguous)
	assert.False(backendErr.Retryable)
	assert.Equal(int64(1), dispatched.Load())
}

func TestHubWorkspaceRefreshUsesProviderMutationBoundary(t *testing.T) {
	var gotScope federationauth.Scope
	var gotPath string
	source := &hubProviderSource{client: providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		gotScope = scope
		gotPath = request.URL.Path
		return nil, providerplane.ErrHubUnavailable
	})}

	_, _ = source.RefreshWorkspaceLaunchSpec(t.Context(), db.WorkspaceLaunchSpec{
		Repository: db.WorkspaceLaunchRepository{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		},
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 7,
		ItemKey: "7", GitHeadRef: "feature/seven",
	})

	assert.Equal(t, federationauth.ScopeProviderWrite, gotScope)
	assert.Equal(t, "/api/v1/federation/provider/workspace-launch-spec/refresh", gotPath)
}

func TestHubPullCandidatesUseProviderQualifiedRepositoryFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	row := pullapi.MergeRequestResponse{
		Number: 7, State: db.MergeRequestStateOpen,
		Repo: httpapi.RepoRefResponse{
			Provider: "gitlab", PlatformHost: "gitlab.example.com",
			RepoPath: "group/project", Owner: "group", Name: "project",
		},
		RepoOwner: "group", RepoName: "project", PlatformHost: "gitlab.example.com",
	}
	encoded, err := json.Marshal([]pullapi.MergeRequestResponse{row})
	require.NoError(err)
	var gotRepo string
	source := &hubProviderSource{client: providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		assert.Equal(federationauth.ScopeProviderRead, scope)
		gotRepo = request.URL.Query().Get("repo")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(encoded)),
			Request:    request,
		}, nil
	})}

	candidates, err := source.ListOpenPullCandidates(t.Context(), workspace.Workspace{
		Platform: "gitlab", PlatformHost: "gitlab.example.com",
		RepoOwner: "group", RepoName: "project",
	})

	require.NoError(err)
	assert.Equal("gitlab|gitlab.example.com/group/project", gotRepo)
	require.Len(candidates, 1)
	assert.Equal(7, candidates[0].Number)
}

func TestHubListFiltersForwardUnassigned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	seen := make(map[string]bool)
	source := &hubProviderSource{client: providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		assert.Equal(federationauth.ScopeProviderRead, scope)
		seen[request.URL.Path] = request.URL.Query().Get("unassigned") == "true"
		body := "[]"
		if request.URL.Path == "/api/v1/activity" {
			body = "{}"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Request:    request,
		}, nil
	})}

	_, err := source.ListPulls(t.Context(), pullapi.ListQuery{Unassigned: true})
	require.NoError(err)
	_, err = source.ListIssues(t.Context(), issueapi.ListQuery{Unassigned: true})
	require.NoError(err)
	_, err = source.ListActivity(t.Context(), &listActivityInput{Unassigned: true})
	require.NoError(err)

	assert.Equal(map[string]bool{
		"/api/v1/pulls": true, "/api/v1/issues": true, "/api/v1/activity": true,
	}, seen)
}

func TestHubUnassignedActivitySubjectFilterBatchesLargeSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const subjectCount = 11_000
	var requestCount int
	source := &hubProviderSource{client: providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		assert.Equal(federationauth.ScopeProviderRead, scope)
		assert.Equal("/api/v1/federation/provider/activity/unassigned-subjects/query", request.URL.Path)
		var body federationUnassignedActivitySubjectsRequest
		require.NoError(json.NewDecoder(request.Body).Decode(&body))
		assert.LessOrEqual(len(body.Subjects), 500)
		requestCount++
		encoded, err := json.Marshal(federationUnassignedActivitySubjectsResponse(body))
		require.NoError(err)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(encoded)),
			Request:    request,
		}, nil
	})}

	subjects := make([]providerplane.ItemIdentity, subjectCount)
	for i := range subjects {
		subjects[i] = providerplane.ItemIdentity{
			Repository: providerplane.RepositoryIdentity{
				Provider: "github", PlatformHost: "github.com", PlatformRepoID: "repo-acme-widget",
			},
			ItemType: "pr", ItemNumber: i + 1,
		}
	}

	got, err := source.FilterUnassignedActivitySubjects(t.Context(), subjects)
	require.NoError(err)
	assert.Equal(22, requestCount)
	assert.Equal(subjects, got)
}

func TestSpokeUnassignedActivityKeepsMatchingLocalWorkspaceSubject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, database := setupTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	unassignedID := seedPR(t, database, "acme", "widget", 1)
	assignedID := seedPR(t, database, "acme", "widget", 2)
	repo, err := database.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	require.NoError(database.UpdateMergeRequestAssignees(t.Context(), repo.ID, unassignedID, nil))
	require.NoError(database.UpdateMergeRequestAssignees(t.Context(), repo.ID, assignedID, []string{"reviewer"}))

	snapshot := workspaceapi.WorkspaceSubjectSnapshot{
		OwnReferences: map[db.WorkspaceSubjectKey]workspaceapi.WorkspaceRef{},
		Subjects:      map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{},
	}
	for number, workspaceID := range map[int]string{1: "ws-unassigned", 2: "ws-assigned"} {
		key := db.WorkspaceSubjectKey{
			RepoID: repo.ID, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: number,
		}
		snapshot.Subjects[key] = workspaceapi.SubjectActivity{
			Subject: db.WorkspaceSubjectMetadata{
				Key: key, Platform: repo.Platform, PlatformHost: repo.PlatformHost,
				PlatformRepoID: repo.PlatformRepoID, RepoOwner: repo.Owner, RepoName: repo.Name,
				RepoPath: repo.RepoPath, Title: "Local workspace", State: "open",
				URL: "https://github.com/acme/widget/pull/1", Author: "author",
			},
			Workspace:  workspaceapi.WorkspaceRef{ID: workspaceID, Status: "ready"},
			ActivityAt: &now,
		}
	}

	response, err := srv.overlayLocalActivityWorkspaceSnapshot(
		t.Context(),
		&listActivityInput{Unassigned: true},
		activityResponse{UseWorkspaceActivityForRecency: true},
		snapshot,
	)
	require.NoError(err)
	require.Len(response.WorkspaceActivity, 1)
	assert.Equal(1, response.WorkspaceActivity[0].ItemNumber)
	assert.Equal("ws-unassigned", response.WorkspaceActivity[0].Workspace.ID)
}

func TestSpokeUnassignedActivityUsesHubAssignmentWithoutLocalProviderRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hub, hubDatabase := setupTestServer(t)
	unassignedIssueID := seedIssue(t, hubDatabase, "acme", "widget", 7, "open")
	assignedIssueID := seedIssue(t, hubDatabase, "acme", "widget", 8, "open")
	hubRepo, err := hubDatabase.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(hubRepo)
	require.NoError(hubDatabase.UpdateIssueAssignees(
		t.Context(), hubRepo.ID, unassignedIssueID, nil,
	))
	require.NoError(hubDatabase.UpdateIssueAssignees(
		t.Context(), hubRepo.ID, assignedIssueID, []string{"reviewer"},
	))

	spoke, spokeDatabase := setupTestServer(t)
	spokeRepoID, err := spokeDatabase.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	spoke.providerSource = &hubProviderSource{client: providerPlaneClientFunc(func(
		_ context.Context, scope federationauth.Scope, request *http.Request,
	) (*http.Response, error) {
		assert.Equal(federationauth.ScopeProviderRead, scope)
		request.Host = "127.0.0.1"
		request.RemoteAddr = "127.0.0.1:1234"
		recorder := httptest.NewRecorder()
		hub.ServeHTTP(recorder, request)
		assert.Equal(http.StatusOK, recorder.Code, recorder.Body.String())
		return recorder.Result(), nil
	})}

	snapshot := workspaceapi.WorkspaceSubjectSnapshot{
		OwnReferences: map[db.WorkspaceSubjectKey]workspaceapi.WorkspaceRef{
			{
				RepoID: spokeRepoID, ItemType: db.WorkspaceItemTypeIssue, ItemNumber: 7,
			}: {ID: "ws-issue", Status: "ready"},
			{
				RepoID: spokeRepoID, ItemType: db.WorkspaceItemTypeIssue, ItemNumber: 8,
			}: {ID: "ws-assigned-issue", Status: "ready"},
		},
		Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{},
	}

	response, err := spoke.overlayLocalActivityWorkspaceSnapshot(
		t.Context(),
		&listActivityInput{Unassigned: true},
		activityResponse{Items: []activityItemResponse{
			{
				Repo: activityRepoRefResponse{
					Provider: "github", PlatformHost: "github.com",
					PlatformRepoID: "repo-acme-widget",
				},
				ItemType: "issue", ItemNumber: 7,
			},
			{
				Repo: activityRepoRefResponse{
					Provider: "github", PlatformHost: "github.com",
					PlatformRepoID: "repo-acme-widget",
				},
				ItemType: "issue", ItemNumber: 8,
			},
		},
			UseWorkspaceActivityForRecency: true,
		},
		snapshot,
	)
	require.NoError(err)
	require.Len(response.Items, 2)
	require.NotNil(response.Items[0].Workspace)
	assert.Equal("ws-issue", response.Items[0].Workspace.ID)
	assert.Nil(response.Items[1].Workspace)
	require.Empty(response.WorkspaceActivity)
}

func TestNodeServerRoutesProviderReadsWithoutUsingLocalTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	unreachable := "https://" + listener.Addr().String()
	require.NoError(listener.Close())
	credentials, err := federationauth.Open(t.TempDir() + "/credentials.json")
	require.NoError(err)
	require.NoError(credentials.StoreOutbound(
		proxyTestHubID,
		"provider-proxy-secret",
		federationauth.SpokeToHubScopes(),
	))
	cfg := &config.Config{Fleet: config.Fleet{
		Enabled: true,
		Role:    config.FleetRoleSpoke,
		Hub: &config.FleetHub{
			NodeID: proxyTestHubID, BaseURL: unreachable,
		},
	}}
	httpClient := &http.Client{Timeout: 100 * time.Millisecond}
	srv := New(dbtest.Open(t), nil, nil, "/", cfg, ServerOptions{
		FederationSpokeID:                  proxyTestNodeID,
		FederationSpokeActive:              true,
		FederationCredentials:              credentials,
		FederationHTTPClient:               httpClient,
		DisableWorkspaceBackgroundMonitors: true,
	})
	spoke := httptest.NewServer(srv)
	t.Cleanup(spoke.Close)

	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	defer response.Body.Close()
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(httpapi.CodeHubUnavailable, problem.Code)

	_, err = srv.MCPBackend().ListPulls(t.Context(), mcpserver.ItemListQuery{})
	var backendErr *mcpserver.Error
	require.ErrorAs(err, &backendErr)
	assert.Equal("unavailable", backendErr.Kind)
	assert.Equal(string(httpapi.CodeHubUnavailable), backendErr.Code)
}

func TestNodeProviderRoutesStopWhenFleetIsDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var hubReads atomic.Int64
	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, request *http.Request,
	) {
		if request.URL.Path == "/api/v1/pulls" {
			hubReads.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(hub.Close)

	credentials, err := federationauth.Open(t.TempDir() + "/credentials.json")
	require.NoError(err)
	require.NoError(credentials.StoreOutbound(
		proxyTestHubID, "provider-proxy-secret",
		federationauth.SpokeToHubScopes(),
	))
	cfg := &config.Config{Fleet: config.Fleet{
		Enabled: true, Role: config.FleetRoleSpoke,
		Hub: &config.FleetHub{
			NodeID: proxyTestHubID, BaseURL: hub.URL,
		},
	}}
	srv := New(dbtest.Open(t), nil, nil, "/", cfg, ServerOptions{
		FederationSpokeID: proxyTestNodeID, FederationSpokeActive: true,
		FederationCredentials: credentials, FederationHTTPClient: hub.Client(),
		DisableWorkspaceBackgroundMonitors: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	spoke := httptest.NewServer(srv)
	t.Cleanup(spoke.Close)

	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	require.NoError(response.Body.Close())
	require.Equal(http.StatusOK, response.StatusCode)
	require.Equal(int64(1), hubReads.Load())

	srv.cfgMu.Lock()
	srv.cfg.Fleet.Enabled = false
	srv.cfgMu.Unlock()
	response, err = spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	defer response.Body.Close()
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(httpapi.CodeHubUnavailable, problem.Code)
	assert.Equal(int64(1), hubReads.Load())

	_, err = srv.MCPBackend().ListPulls(t.Context(), mcpserver.ItemListQuery{})
	var backendErr *mcpserver.Error
	require.ErrorAs(err, &backendErr)
	assert.Equal(string(httpapi.CodeHubUnavailable), backendErr.Code)
	assert.Equal(int64(1), hubReads.Load())

	srv.cfgMu.Lock()
	srv.cfg.Fleet.Enabled = true
	srv.cfgMu.Unlock()
	response, err = spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	defer response.Body.Close()
	assert.Equal(http.StatusOK, response.StatusCode)
	assert.Equal(int64(2), hubReads.Load())
}

func TestNodeProviderFetchKeepsHubOrderAndAddsOnlyLocalWorkspace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hubDB := dbtest.Open(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	pullID := seedPR(t, hubDB, "acme", "widget", 1,
		withSeedPRTimes(base, base, base))
	seedPR(t, hubDB, "acme", "widget", 2,
		withSeedPRTimes(base.Add(time.Minute), base.Add(time.Minute), base.Add(time.Minute)))
	require.NoError(hubDB.UpsertMREvents(t.Context(), []db.MREvent{{
		MergeRequestID: pullID, EventType: "issue_comment", Author: "reviewer",
		Body: "provider-owned activity", CreatedAt: base.Add(2 * time.Minute),
		DedupeKey: "federated-provider-activity",
	}}))
	hubRepo, err := hubDB.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(hubRepo)

	hubCredentials, err := federationauth.Open(t.TempDir() + "/hub-credentials.json")
	require.NoError(err)
	token, err := hubCredentials.MintInbound(
		proxyTestNodeID, federationauth.SpokeToHubScopes(),
	)
	require.NoError(err)
	hubServer := New(
		hubDB, nil, nil, "/", nil,
		ServerOptions{
			DaemonAccess: DaemonAccessOptions{
				Token: "hub-local-secret", RequireAPIAuth: true,
			},
			FederationSpokeID:                  proxyTestHubID,
			FederationCredentials:              hubCredentials,
			DisableWorkspaceBackgroundMonitors: true,
		},
	)
	t.Cleanup(func() { gracefulShutdown(t, hubServer) })
	hub := httptest.NewTLSServer(hubServer)
	t.Cleanup(hub.Close)

	nodeDB := dbtest.Open(t)
	seedPR(t, nodeDB, "acme", "widget", 1)
	seedWorkspace(
		t, nodeDB, "ws-spoke-only", "acme", "widget",
		db.WorkspaceItemTypePullRequest, 1,
	)
	spokeCredentials, err := federationauth.Open(t.TempDir() + "/spoke-credentials.json")
	require.NoError(err)
	require.NoError(spokeCredentials.StoreOutbound(
		proxyTestHubID, token, federationauth.SpokeToHubScopes(),
	))
	nodeConfig := &config.Config{
		Fleet: config.Fleet{
			Enabled: true, Role: config.FleetRoleSpoke,
			Hub: &config.FleetHub{
				NodeID: proxyTestHubID, BaseURL: hub.URL,
			},
		},
		Tmux: config.Tmux{Command: []string{"kenn-forge-no-such-tmux"}},
	}
	nodeServer := New(nodeDB, nil, nil, "/", nodeConfig, ServerOptions{
		FederationSpokeID:                  proxyTestNodeID,
		FederationSpokeActive:              true,
		FederationCredentials:              spokeCredentials,
		FederationHTTPClient:               hub.Client(),
		DisableWorkspaceBackgroundMonitors: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, nodeServer) })
	spoke := httptest.NewServer(nodeServer)
	t.Cleanup(spoke.Close)

	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls?state=open")
	require.NoError(err)
	defer response.Body.Close()
	require.Equal(http.StatusOK, response.StatusCode)
	var rows []pullapi.MergeRequestResponse
	require.NoError(json.NewDecoder(response.Body).Decode(&rows))
	require.Len(rows, 2)
	assert.Equal([]int{2, 1}, []int{rows[0].Number, rows[1].Number})
	assert.Nil(rows[0].Workspace)
	require.NotNil(rows[1].Workspace)
	assert.Equal("ws-spoke-only", rows[1].Workspace.ID)

	activityURL := spoke.URL + "/api/v1/activity?projection=events&item_types=pr&since=" +
		url.QueryEscape(base.Add(-time.Minute).Format(time.RFC3339))
	activityHTTPResponse, err := spoke.Client().Get(activityURL)
	require.NoError(err)
	defer activityHTTPResponse.Body.Close()
	require.Equal(http.StatusOK, activityHTTPResponse.StatusCode)
	var activity activityResponse
	require.NoError(json.NewDecoder(activityHTTPResponse.Body).Decode(&activity))
	require.NotEmpty(activity.Items)
	for _, item := range activity.Items {
		assert.NotEmpty(item.Cursor)
		if item.ItemNumber == 1 {
			require.NotNil(item.Workspace)
			assert.Equal("ws-spoke-only", item.Workspace.ID)
		} else {
			assert.Nil(item.Workspace)
		}
	}

	mcpPulls, err := nodeServer.MCPBackend().ListPulls(
		t.Context(), mcpserver.ItemListQuery{Repository: mcpserver.RepositoryIdentity{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: hubRepo.PlatformRepoID,
			Owner:          "acme", Name: "widget",
		}},
	)
	require.NoError(err)
	require.Len(mcpPulls, 2)
	assert.Equal([]int{2, 1}, []int{mcpPulls[0].Number, mcpPulls[1].Number})
	assert.Nil(mcpPulls[0].Workspace)
	require.NotNil(mcpPulls[1].Workspace)
	assert.Equal("ws-spoke-only", mcpPulls[1].Workspace.ID)
}

func TestFederatedReviewDraftHasOneHubOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	caps := platform.Capabilities{
		ReadRepositories:       true,
		ReadMergeRequests:      true,
		ReadIssues:             true,
		ReadComments:           true,
		ReviewDraftMutation:    true,
		SupportedReviewActions: []platform.ReviewAction{platform.ReviewActionComment},
	}
	hubServer, hubDB, provider := setupGitLabCapabilityServerWithProvider(t, &caps)
	hub := httptest.NewTLSServer(hubServer)
	t.Cleanup(hub.Close)

	nodeA, nodeADB := newFederatedProviderNodeForTest(
		t, "55555555555555555555555555555555", hub.URL, hub.Client(),
	)
	nodeB, nodeBDB := newFederatedProviderNodeForTest(
		t, "66666666666666666666666666666666", hub.URL, hub.Client(),
	)
	path := "/api/v1/host/gitlab.example.com/pulls/gl/group/project/7/review-draft"
	created := testutil.DoJSON(t, nodeA, http.MethodPost, path+"/comments", map[string]any{
		"body": "comment from spoke A",
		"range": map[string]any{
			"path": "internal/server/provider_proxy_test.go", "side": "right",
			"line": 42, "new_line": 42, "line_type": "add",
			"diff_head_sha": "abc123", "commit_sha": "abc123",
		},
	})

	require.Equal(http.StatusCreated, created.Code, created.Body.String())
	var createdComment struct {
		ID string `json:"id"`
	}
	require.NoError(json.NewDecoder(created.Body).Decode(&createdComment))
	require.NotEmpty(createdComment.ID)

	edited := testutil.DoJSON(t, nodeA, http.MethodPatch, path+"/comments/"+createdComment.ID, map[string]any{
		"body": "edited comment from spoke A",
		"range": map[string]any{
			"path": "internal/server/provider_proxy_test.go", "side": "right",
			"line": 43, "new_line": 43, "line_type": "add",
			"diff_head_sha": "abc123", "commit_sha": "abc123",
		},
	})

	require.Equal(http.StatusOK, edited.Code, edited.Body.String())

	read := testutil.DoJSON(t, nodeB, http.MethodGet, path, nil)
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	var draft map[string]any
	require.NoError(json.NewDecoder(read.Body).Decode(&draft))
	require.Len(draft["comments"], 1)
	comments, ok := draft["comments"].([]any)
	require.True(ok)
	comment, ok := comments[0].(map[string]any)
	require.True(ok)
	assert.Equal("edited comment from spoke A", comment["body"])

	published := testutil.DoJSON(t, nodeB, http.MethodPost, path+"/publish", map[string]any{
		"action": "comment",
	})

	require.Equal(http.StatusOK, published.Code, published.Body.String())
	assert.Len(provider.publishedReviews, 1)

	repo, err := hubDB.GetRepoByIdentity(t.Context(), db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "gitlab.example.com", RepoPath: "group/project",
	})
	require.NoError(err)
	require.NotNil(repo)
	mr, err := hubDB.GetMergeRequestByRepoIDAndNumber(t.Context(), repo.ID, 7)
	require.NoError(err)
	require.NotNil(mr)
	hubDraft, err := hubDB.GetMRReviewDraft(t.Context(), mr.ID)
	require.NoError(err)
	assert.Nil(hubDraft)
	for _, nodeDB := range []*db.DB{nodeADB, nodeBDB} {
		nodeDraft, draftErr := nodeDB.GetMRReviewDraft(t.Context(), mr.ID)
		require.NoError(draftErr)
		assert.Nil(nodeDraft)
	}
}

func TestFederatedWorkflowHasOneHubOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hubDB := dbtest.Open(t)
	seedPR(t, hubDB, "acme", "widget", 42)
	repo, err := hubDB.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	hubServer := New(hubDB, nil, nil, "/", nil, ServerOptions{
		DisableWorkspaceBackgroundMonitors: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, hubServer) })
	hub := httptest.NewTLSServer(hubServer)
	t.Cleanup(hub.Close)

	nodeA, _ := newFederatedProviderNodeForTest(
		t, "77777777777777777777777777777777", hub.URL, hub.Client(),
	)
	nodeB, _ := newFederatedProviderNodeForTest(
		t, "88888888888888888888888888888888", hub.URL, hub.Client(),
	)
	item := mcpserver.ItemIdentity{
		Type: "pr", Provider: "github", PlatformHost: "github.com",
		PlatformRepoID: repo.PlatformRepoID, Owner: "acme", Name: "widget", Number: 42,
	}
	mutation, err := nodeA.MCPBackend().SetWorkflowState(t.Context(), item, mcpserver.WorkflowUpdate{
		Status: "reviewing", ExpectedStatus: "new", Source: "mcp", Actor: "spoke-a",
	})
	require.NoError(err)
	assert.Equal("reviewing", mutation.State.Status)

	page, err := nodeB.MCPBackend().ListWorkflowStates(t.Context(), mcpserver.WorkflowQuery{
		Repository: mcpserver.RepositoryIdentity{
			Provider: "github", PlatformHost: "github.com",
			PlatformRepoID: repo.PlatformRepoID, Owner: "acme", Name: "widget",
		},
		ItemTypes: []string{"pr"},
	})
	require.NoError(err)
	require.Len(page.Items, 1)
	assert.Equal("reviewing", page.Items[0].Workflow.Status)

	state, err := hubDB.GetItemWorkflowState(t.Context(), repo.ID, db.ItemTypePR, 42)
	require.NoError(err)
	require.NotNil(state)
	assert.Equal(string(db.KanbanStatusReviewing), state.Status)
}

func newFederatedProviderNodeForTest(
	t *testing.T,
	nodeID, hubURL string,
	httpClient *http.Client,
) (*Server, *db.DB) {
	t.Helper()
	credentials, err := federationauth.Open(t.TempDir() + "/credentials.json")
	require.NoError(t, err)
	require.NoError(t, credentials.StoreOutbound(
		proxyTestHubID, "provider-spoke-secret", federationauth.SpokeToHubScopes(),
	))
	cfg := &config.Config{Fleet: config.Fleet{
		Enabled: true, Role: config.FleetRoleSpoke,
		Hub: &config.FleetHub{
			NodeID: proxyTestHubID, BaseURL: hubURL,
		},
	}}
	database := dbtest.Open(t)
	server := New(database, nil, nil, "/", cfg, ServerOptions{
		FederationSpokeID:                  nodeID,
		FederationSpokeActive:              true,
		FederationCredentials:              credentials,
		FederationHTTPClient:               httpClient,
		DisableWorkspaceBackgroundMonitors: true,
	})
	t.Cleanup(func() { gracefulShutdown(t, server) })
	return server, database
}

func TestProviderProxyMapsHubTimeoutToUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		<-r.Context().Done()
	}))
	t.Cleanup(hub.Close)

	httpClient := hub.Client()
	httpClient.Timeout = 25 * time.Millisecond
	spoke := newProviderProxyTestServer(t, hub, httpClient, nil)
	response, err := spoke.Client().Get(spoke.URL + "/api/v1/pulls")
	require.NoError(err)
	defer response.Body.Close()
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))
	assert.Equal(http.StatusServiceUnavailable, response.StatusCode)
	assert.Equal(httpapi.CodeHubUnavailable, problem.Code)
}

type providerDispatchTestHandler struct {
	hubURL          string
	httpClient      *http.Client
	local           http.Handler
	credentialStore *federationauth.Store
}

func (h *providerDispatchTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	client, err := providerplane.NewClient(providerplane.Options{
		LocalNodeID: proxyTestNodeID,
		Hub: providerplane.Hub{
			NodeID:  proxyTestHubID,
			BaseURL: h.hubURL,
		},
		Credentials: h.credentialStore,
		HTTPClient:  h.httpClient,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxy := newProviderProxy(client)
	rule, ok := providerRouteRuleForRequest(r.Method, r.URL.Path)
	if ok && rule.Owner != NodeLocal {
		proxy.ServeHTTP(w, r, rule)
		return
	}
	h.local.ServeHTTP(w, r)
}

func newProviderProxyTestServer(
	t *testing.T,
	hub *httptest.Server,
	httpClient *http.Client,
	local http.Handler,
) *httptest.Server {
	t.Helper()
	if local == nil {
		local = http.NotFoundHandler()
	}
	credentials, err := federationauth.Open(t.TempDir() + "/credentials.json")
	require.NoError(t, err)
	require.NoError(t, credentials.StoreOutbound(
		proxyTestHubID,
		"provider-proxy-secret",
		federationauth.SpokeToHubScopes(),
	))
	hubURL := "https://127.0.0.1:1"
	if hub != nil {
		hubURL = hub.URL
	}
	handler := &providerDispatchTestHandler{
		hubURL: hubURL, httpClient: httpClient,
		local: local, credentialStore: credentials,
	}
	spoke := httptest.NewTLSServer(handler)
	t.Cleanup(spoke.Close)
	return spoke
}

func TestProviderProxyHonorsCallerCancellation(t *testing.T) {
	runParallelServerTest(t)

	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		<-r.Context().Done()
	}))
	t.Cleanup(hub.Close)
	spoke := newProviderProxyTestServer(t, hub, hub.Client(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, spoke.URL+"/api/v1/pulls", nil,
	)
	require.NoError(t, err)
	cancel()
	_, err = spoke.Client().Do(request)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestProviderProxyRejectsOversizedHubResponse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	runParallelServerTest(t)

	hub := httptest.NewTLSServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = io.WriteString(w, "four")
	}))
	t.Cleanup(hub.Close)
	credentials, err := federationauth.Open(t.TempDir() + "/credentials.json")
	require.NoError(err)
	require.NoError(credentials.StoreOutbound(
		proxyTestHubID,
		"response-limit-secret",
		federationauth.SpokeToHubScopes(),
	))
	client, err := providerplane.NewClient(providerplane.Options{
		LocalNodeID: proxyTestNodeID,
		Hub: providerplane.Hub{
			NodeID:  proxyTestHubID,
			BaseURL: hub.URL,
		},
		Credentials: credentials,
		HTTPClient:  hub.Client(),
	})
	require.NoError(err)
	proxy := newProviderProxy(client)
	proxy.responseBodyLimit = 3
	rule, ok := providerRouteRuleForRequest(http.MethodGet, "/api/v1/pulls")
	require.True(ok)
	spoke := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r, rule)
	}))
	t.Cleanup(spoke.Close)

	request, err := http.NewRequest(http.MethodGet, spoke.URL+"/api/v1/pulls", nil)
	require.NoError(err)
	response, err := spoke.Client().Do(request)
	require.NoError(err)
	defer response.Body.Close()
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(response.Body).Decode(&problem))

	assert.Equal(http.StatusBadGateway, response.StatusCode)
	assert.Equal(httpapi.CodeUpstreamError, problem.Code)
}

func TestProviderProxyReportsUnknownWriteOutcomeWhenResponseBufferingFails(t *testing.T) {
	tests := []struct {
		name  string
		body  io.ReadCloser
		limit int64
	}{
		{
			name: "truncated",
			body: &failingProviderResponseBody{
				data: []byte(`{"ok":`), err: io.ErrUnexpectedEOF,
			},
			limit: 32,
		},
		{
			name:  "oversized",
			body:  io.NopCloser(bytes.NewBufferString("four")),
			limit: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := providerPlaneClientFunc(func(
				context.Context, federationauth.Scope, *http.Request,
			) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Body: test.body,
				}, nil
			})
			proxy := newProviderProxy(client)
			proxy.responseBodyLimit = test.limit
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/provider-write", nil)

			proxy.ServeHTTP(recorder, request, ProviderRouteRule{
				PeerScope: federationauth.ScopeProviderWrite,
			})

			var problem httpapi.ProblemError
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
			assert.Equal(t, http.StatusBadGateway, recorder.Code)
			assert.Equal(t, httpapi.CodeMutationOutcomeUnknown, problem.Code)
		})
	}
}
