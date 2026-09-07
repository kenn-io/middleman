package e2etest

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/fleet"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/mcpserver"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/pullapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/federationtest"
	"go.kenn.io/forge/platform"
)

const (
	federatedHubID   = "10101010101010101010101010101010"
	federatedNodeAID = "20202020202020202020202020202020"
	federatedNodeBID = "30303030303030303030303030303030"
)

type federationHandlerBox struct {
	handler http.Handler
}

// switchableFederationHandler lets the fixture model a hub outage
// without changing an origin or rebuilding either spoke's HTTP client.
type switchableFederationHandler struct {
	current atomic.Pointer[federationHandlerBox]
	offline atomic.Bool
}

func newSwitchableFederationHandler() *switchableFederationHandler {
	handler := &switchableFederationHandler{}
	handler.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "federation daemon is starting", http.StatusServiceUnavailable)
	}))
	return handler
}

func (h *switchableFederationHandler) Set(handler http.Handler) {
	h.current.Store(&federationHandlerBox{handler: handler})
}

func (h *switchableFederationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.offline.Load() {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": http.StatusServiceUnavailable,
			"code":   httpapi.CodeHubUnavailable,
			"detail": "the federation hub is unavailable",
		})
		return
	}
	h.current.Load().handler.ServeHTTP(w, r)
}

type federatedDaemonFixture struct {
	NodeID      string
	Name        string
	LocalToken  string
	Database    *db.DB
	Credentials *federationauth.Store
	Enrollments *federation.Store
	Server      *server.Server
	HTTP        *httptest.Server
	Switch      *switchableFederationHandler
}

type countingSyntheticProvider struct {
}

func (p *countingSyntheticProvider) Seed(t *testing.T, database *db.DB) int64 {
	t.Helper()
	identity := verifiedRepoIdentity(db.GitHubRepoIdentity(
		"github.com", "acme", "widget",
	))
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(t, err)
	require.NoError(t, database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			PlatformRepoID: identity.PlatformRepoID,
			WebURL:         "https://github.com/acme/widget",
			CloneURL:       "https://github.com/acme/widget.git",
			DefaultBranch:  "main",
		},
	))
	now := time.Now().UTC().Truncate(time.Second)
	for _, pull := range []db.MergeRequest{
		{
			RepoID: repoID, PlatformID: 1001, PlatformExternalID: "pull-1",
			Number: 1, URL: "https://github.com/acme/widget/pull/1",
			Title: "Draft federation", Author: "ada", State: db.MergeRequestStateOpen,
			IsDraft: true, HeadBranch: "draft-federation", BaseBranch: "main",
			SnapshotRevision: 1, CreatedAt: now.Add(-2 * time.Hour),
			UpdatedAt: now.Add(-2 * time.Hour), LastActivityAt: now.Add(-2 * time.Hour),
		},
		{
			RepoID: repoID, PlatformID: 1002, PlatformExternalID: "pull-2",
			Number: 2, URL: "https://github.com/acme/widget/pull/2",
			Title: "Newer federation", Author: "grace", State: db.MergeRequestStateOpen,
			HeadBranch: "newer-federation", BaseBranch: "main",
			SnapshotRevision: 1, CreatedAt: now.Add(-time.Hour),
			UpdatedAt: now.Add(-time.Hour), LastActivityAt: now.Add(-time.Hour),
		},
	} {
		_, err := database.UpsertMergeRequest(t.Context(), &pull)
		require.NoError(t, err)
	}
	return repoID
}

type federatedForgesFixture struct {
	Hub        *federatedDaemonFixture
	NodeA      *federatedDaemonFixture
	NodeB      *federatedDaemonFixture
	Provider   *countingSyntheticProvider
	HTTPClient *http.Client
	nodeAToken string
	nodeBToken string
}

func newFederatedForgesFixture(t *testing.T) *federatedForgesFixture {
	t.Helper()

	hub := newFederatedDaemonOrigin(t, federatedHubID, "hub")
	nodeA := newFederatedDaemonOrigin(t, federatedNodeAID, "spoke-a")
	nodeB := newFederatedDaemonOrigin(t, federatedNodeBID, "spoke-b")
	client := federationTLSClient(t, hub.HTTP, nodeA.HTTP, nodeB.HTTP)

	nodeAToken := connectFederationCredentials(
		t, hub, nodeA,
	)
	nodeBToken := connectFederationCredentials(
		t, hub, nodeB,
	)

	provider := &countingSyntheticProvider{}
	provider.Seed(t, hub.Database)
	seedFederatedNodeRepository(t, nodeA.Database)
	seedFederatedNodeRepository(t, nodeB.Database)
	seedFederatedWorkspace(t, nodeA.Database, "ws-spoke-a", 1, "draft-federation")
	seedFederatedWorkspace(t, nodeB.Database, "ws-spoke-b", 2, "newer-federation")

	hubConfig := &config.Config{
		BasePath: "/",
		Tmux:     config.Tmux{Command: []string{"kenn-forge-no-such-tmux"}},
		Repos: []config.Repo{{
			Platform: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget",
		}},
		Fleet: config.Fleet{
			Enabled: true, Role: config.FleetRoleHub, PeerTimeout: "500ms",
			Members: []config.FleetMember{
				{NodeID: nodeA.NodeID, Name: nodeA.Name, BaseURL: nodeA.HTTP.URL, State: federation.EnrollmentActive},
				{NodeID: nodeB.NodeID, Name: nodeB.Name, BaseURL: nodeB.HTTP.URL, State: federation.EnrollmentActive},
			},
		},
	}
	hub.Server = newFederatedDaemonServer(
		t, hub, hubConfig, client, false,
	)
	hub.Switch.Set(hub.Server)

	for _, spoke := range []*federatedDaemonFixture{nodeA, nodeB} {
		nodeConfig := &config.Config{
			BasePath: "/",
			Tmux:     config.Tmux{Command: []string{"kenn-forge-no-such-tmux"}},
			Fleet: config.Fleet{
				Enabled: true, Role: config.FleetRoleSpoke, PeerTimeout: "500ms",
				Hub: &config.FleetHub{
					NodeID: hub.NodeID, Name: hub.Name,
					BaseURL: hub.HTTP.URL,
				},
			},
		}
		spoke.Server = newFederatedDaemonServer(t, spoke, nodeConfig, client, true)
		spoke.Switch.Set(spoke.Server)
	}

	return &federatedForgesFixture{
		Hub: hub, NodeA: nodeA, NodeB: nodeB,
		Provider: provider, HTTPClient: client,
		nodeAToken: nodeAToken, nodeBToken: nodeBToken,
	}
}

func newFederatedDaemonOrigin(
	t *testing.T, nodeID, name string,
) *federatedDaemonFixture {
	t.Helper()
	handler := newSwitchableFederationHandler()
	httpServer := httptest.NewUnstartedServer(handler)
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	credentials, err := federationauth.Open(filepath.Join(
		t.TempDir(), "federation-credentials.json",
	))
	require.NoError(t, err)
	enrollments, err := federation.Open(
		filepath.Join(t.TempDir(), "federation-enrollments.json"),
		federation.StoreOptions{},
	)
	require.NoError(t, err)
	return &federatedDaemonFixture{
		NodeID: nodeID, Name: name, LocalToken: name + "-local-secret",
		Database: dbtest.Open(t), Credentials: credentials, Enrollments: enrollments,
		HTTP: httpServer, Switch: handler,
	}
}

func federationTLSClient(t *testing.T, origins ...*httptest.Server) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	for _, origin := range origins {
		require.NotNil(t, origin.Certificate())
		roots.AddCert(origin.Certificate())
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12,
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// connectFederationCredentials installs both directed credentials and returns
// the spoke-to-hub bearer so revocation can be asserted later.
func connectFederationCredentials(
	t *testing.T,
	hub, spoke *federatedDaemonFixture,
) string {
	t.Helper()
	enrollment, err := federationtest.SeedActiveHubEnrollment(
		t.Context(), hub.Enrollments,
		federation.Identity{
			NodeID: hub.NodeID, Name: hub.Name, BaseURL: hub.HTTP.URL,
		},
		federation.Identity{
			NodeID: spoke.NodeID, Name: spoke.Name, BaseURL: spoke.HTTP.URL,
		},
		spoke.NodeID,
	)
	require.NoError(t, err)
	require.NoError(t, federationtest.SeedActiveSpokeEnrollment(
		t.Context(), spoke.Enrollments, enrollment,
	))
	spokeToHub, err := hub.Credentials.MintInbound(
		spoke.NodeID, federationauth.SpokeToHubScopes(),
	)
	require.NoError(t, err)
	require.NoError(t, spoke.Credentials.StoreOutbound(
		hub.NodeID, spokeToHub,
		federationauth.SpokeToHubScopes(),
	))
	hubToSpoke, err := spoke.Credentials.MintInbound(
		hub.NodeID, federationauth.HubToSpokeScopes(),
	)
	require.NoError(t, err)
	require.NoError(t, hub.Credentials.StoreOutbound(
		spoke.NodeID, hubToSpoke,
		federationauth.HubToSpokeScopes(),
	))
	return spokeToHub
}

func newFederatedDaemonServer(
	t *testing.T,
	daemon *federatedDaemonFixture,
	cfg *config.Config,
	client *http.Client,
	activeSpoke bool,
) *server.Server {
	t.Helper()
	var syncer *ghclient.Syncer
	if !activeSpoke {
		syncer = ghclient.NewSyncer(
			nil, daemon.Database, nil, []ghclient.RepoRef{{
				Platform: platform.KindGitHub, PlatformHost: "github.com",
				Owner: "acme", Name: "widget", RepoPath: "acme/widget",
				PlatformExternalID: verifiedRepoIdentity(db.GitHubRepoIdentity(
					"github.com", "acme", "widget",
				)).PlatformRepoID,
			}}, time.Minute, nil, nil,
		)
		t.Cleanup(syncer.Stop)
	}
	srv := server.New(daemon.Database, syncer, nil, "/", cfg, server.ServerOptions{
		DaemonAccess: server.DaemonAccessOptions{
			Token: daemon.LocalToken, RequireAPIAuth: true,
		},
		FederationSpokeID: daemon.NodeID, FederationSpokeActive: activeSpoke,
		FederationCredentials:              daemon.Credentials,
		FederationEnrollments:              daemon.Enrollments,
		FederationHTTPClient:               client,
		WorktreeDir:                        filepath.Join(t.TempDir(), "worktrees"),
		DisableWorkspaceBackgroundMonitors: true,
		HostCheck: server.HostCheckOptions{
			Bind:                 config.HostKey{Host: "127.0.0.1", Port: "8091"},
			AllowLoopbackAnyPort: true,
		},
	})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	return srv
}

func seedFederatedNodeRepository(t *testing.T, database *db.DB) {
	t.Helper()
	identity := verifiedRepoIdentity(db.GitHubRepoIdentity(
		"github.com", "acme", "widget",
	))
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(t, err)
	require.NoError(t, database.UpdateRepoProviderMetadata(
		t.Context(), repoID, db.RepoProviderMetadata{
			PlatformRepoID: identity.PlatformRepoID,
			WebURL:         "https://github.com/acme/widget",
			CloneURL:       "https://github.com/acme/widget.git",
			DefaultBranch:  "main",
		},
	))
}

func seedFederatedWorkspace(
	t *testing.T, database *db.DB,
	id string, number int, branch string,
) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	workspace := &db.Workspace{
		ID: id, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: number,
		ItemKey: fmt.Sprint(number), GitHeadRef: branch, WorkspaceBranch: branch,
		WorktreePath: filepath.Join(t.TempDir(), id), Status: "ready", CreatedAt: now,
	}
	require.NoError(t, database.CreateWorkspaceWithLaunchSpec(
		t.Context(), workspace, db.WorkspaceLaunchSpec{
			Version: db.WorkspaceLaunchSpecVersion,
			Repository: db.WorkspaceLaunchRepository{
				Provider: "github", PlatformHost: "github.com",
				PlatformRepoID: verifiedRepoIdentity(db.GitHubRepoIdentity(
					"github.com", "acme", "widget",
				)).PlatformRepoID,
				Owner: "acme", Name: "widget",
				CloneURL: "https://github.com/acme/widget.git", DefaultBranch: "main",
			},
			ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: number,
			ItemKey: fmt.Sprint(number), GitHeadRef: branch,
			Pull: &db.WorkspaceLaunchPull{
				HeadBranch: branch, HeadRepoKind: "same_repo", SnapshotRevision: 1,
			},
			SourceVisible: true, IssuedAt: now,
			SourceVisibleUntil: now.Add(db.WorkspaceLaunchSpecVisibilityLease),
		},
	))
}

func (f *federatedForgesFixture) request(
	t *testing.T,
	daemon *federatedDaemonFixture,
	method, path string,
	body io.Reader,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(), method, daemon.HTTP.URL+path, body,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+daemon.LocalToken)
	response, err := f.HTTPClient.Do(request)
	require.NoError(t, err)
	return response
}

func (f *federatedForgesFixture) pulls(
	t *testing.T, daemon *federatedDaemonFixture,
) []pullapi.MergeRequestResponse {
	t.Helper()
	response := f.request(t, daemon, http.MethodGet, "/api/v1/pulls?state=open", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var pulls []pullapi.MergeRequestResponse
	require.NoError(t, json.Unmarshal(body, &pulls))
	return pulls
}

func (f *federatedForgesFixture) snapshot(
	t *testing.T, daemon *federatedDaemonFixture,
) fleet.Snapshot {
	t.Helper()
	response := f.request(
		t, daemon, http.MethodGet, "/api/v1/snapshot?include_peers=true", nil,
	)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var snapshot fleet.Snapshot
	require.NoError(t, json.Unmarshal(body, &snapshot))
	return snapshot
}

func pullNumbers(rows []pullapi.MergeRequestResponse) []int {
	numbers := make([]int, len(rows))
	for index := range rows {
		numbers[index] = rows[index].Number
	}
	return numbers
}

func pullByNumber(
	rows []pullapi.MergeRequestResponse, number int,
) *pullapi.MergeRequestResponse {
	for index := range rows {
		if rows[index].Number == number {
			return &rows[index]
		}
	}
	return nil
}

func workspaceByID(rows []fleet.WorkspaceSummary, id string) *fleet.WorkspaceSummary {
	for index := range rows {
		if rows[index].ID == id {
			return &rows[index]
		}
	}
	return nil
}

// TestFederatedForgesE2E exercises the contracts that only emerge when three
// real daemons communicate: shared provider ownership, observer-local
// workspace overlays, one-hop fleet projection, outage behavior, event
// re-stamping, protocol enforcement, and synchronous credential revocation.
func TestFederatedForgesE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newFederatedForgesFixture(t)

	hubPulls := fixture.pulls(t, fixture.Hub)
	nodeAPulls := fixture.pulls(t, fixture.NodeA)
	nodeBPulls := fixture.pulls(t, fixture.NodeB)
	for _, rows := range [][]pullapi.MergeRequestResponse{
		hubPulls, nodeAPulls, nodeBPulls,
	} {
		assert.Equal([]int{2, 1}, pullNumbers(rows),
			"every daemon preserves hub ordering")
	}
	assert.Nil(pullByNumber(hubPulls, 1).Workspace)
	assert.Nil(pullByNumber(hubPulls, 2).Workspace)
	require.NotNil(pullByNumber(nodeAPulls, 1).Workspace)
	assert.Equal("ws-spoke-a", pullByNumber(nodeAPulls, 1).Workspace.ID)
	assert.Nil(pullByNumber(nodeAPulls, 2).Workspace)
	require.NotNil(pullByNumber(nodeBPulls, 2).Workspace)
	assert.Equal("ws-spoke-b", pullByNumber(nodeBPulls, 2).Workspace.ID)
	assert.Nil(pullByNumber(nodeBPulls, 1).Workspace)
	assert.True(pullByNumber(nodeAPulls, 1).IsDraft)

	hubView := fixture.snapshot(t, fixture.Hub)
	assert.Equal(federation.ProtocolVersion, hubView.ProtocolVersion)
	require.Len(hubView.Hosts, 3)
	require.NotNil(workspaceByID(hubView.Workspaces, "ws-spoke-a"))
	require.NotNil(workspaceByID(hubView.Workspaces, "ws-spoke-b"))

	nodeAView := fixture.snapshot(t, fixture.NodeA)
	assert.Equal(federation.ProtocolVersion, nodeAView.ProtocolVersion)
	require.Len(nodeAView.Hosts, 3)
	nodeAWorkspace := workspaceByID(nodeAView.Workspaces, "ws-spoke-a")
	require.NotNil(nodeAWorkspace)
	assert.Empty(nodeAWorkspace.FleetHostKey,
		"the connected spoke projects its own workspace as local")
	assert.Nil(workspaceByID(nodeAView.Workspaces, "ws-spoke-b"))

	nodeBView := fixture.snapshot(t, fixture.NodeB)
	assert.Equal(federation.ProtocolVersion, nodeBView.ProtocolVersion)
	require.Len(nodeBView.Hosts, 3)
	nodeBWorkspace := workspaceByID(nodeBView.Workspaces, "ws-spoke-b")
	require.NotNil(nodeBWorkspace)
	assert.Empty(nodeBWorkspace.FleetHostKey)
	assert.Nil(workspaceByID(nodeBView.Workspaces, "ws-spoke-a"))

	proxied := fixture.request(
		t, fixture.Hub, http.MethodGet,
		"/api/v1/fleet/hosts/"+federatedNodeAID+"/workspaces", nil,
	)
	proxiedBody, err := io.ReadAll(proxied.Body)
	proxied.Body.Close()
	require.NoError(err)
	require.Equal(http.StatusOK, proxied.StatusCode, string(proxiedBody))
	assert.Contains(string(proxiedBody), "ws-spoke-a")
	assert.NotContains(string(proxiedBody), "ws-spoke-b")

	for _, daemon := range []*federatedDaemonFixture{fixture.NodeA, fixture.NodeB} {
		pulls, err := daemon.Server.MCPBackend().ListPulls(
			t.Context(), mcpserver.ItemListQuery{State: "open"},
		)
		require.NoError(err)
		assert.Equal([]int{2, 1}, []int{pulls[0].Number, pulls[1].Number})
	}
	item := mcpserver.ItemIdentity{
		Type: "pr", Provider: "github", PlatformHost: "github.com",
		PlatformRepoID: verifiedRepoIdentity(db.GitHubRepoIdentity(
			"github.com", "acme", "widget",
		)).PlatformRepoID,
		Owner: "acme", Name: "widget", Number: 1,
	}
	mutation, err := fixture.NodeA.Server.MCPBackend().SetWorkflowState(
		t.Context(), item, mcpserver.WorkflowUpdate{
			Status: "reviewing", ExpectedStatus: "new", Source: "mcp", Actor: "spoke-a",
		},
	)
	require.NoError(err)
	assert.Equal("reviewing", mutation.State.Status)
	workflow, err := fixture.NodeB.Server.MCPBackend().ListWorkflowStates(
		t.Context(), mcpserver.WorkflowQuery{
			Repository: mcpRepositoryIdentity(item), ItemTypes: []string{"pr"},
		},
	)
	require.NoError(err)
	require.Len(workflow.Items, 2)
	var workflowStatuses []string
	for _, row := range workflow.Items {
		if row.Identity.Number == 1 {
			workflowStatuses = append(workflowStatuses, row.Workflow.Status)
		}
	}
	assert.Equal([]string{"reviewing"}, workflowStatuses)

	require.Eventually(func() bool {
		records, _ := fixture.NodeA.Server.Hub().RingSnapshotSince(0)
		return slices.ContainsFunc(records, func(record server.RecordedEvent) bool {
			return record.Event.Type == "hub_connection_changed"
		})
	}, 3*time.Second, 10*time.Millisecond)
	fixture.Hub.Switch.offline.Store(true)
	fixture.Hub.HTTP.CloseClientConnections()
	outage := fixture.request(t, fixture.NodeA, http.MethodGet, "/api/v1/pulls?state=open", nil)
	outageBody, err := io.ReadAll(outage.Body)
	outage.Body.Close()
	require.NoError(err)
	assert.Equal(http.StatusServiceUnavailable, outage.StatusCode, string(outageBody))
	local := fixture.request(t, fixture.NodeA, http.MethodGet, "/api/v1/workspaces", nil)
	localBody, err := io.ReadAll(local.Body)
	local.Body.Close()
	require.NoError(err)
	assert.Equal(http.StatusOK, local.StatusCode, string(localBody))
	assert.Contains(string(localBody), "ws-spoke-a")

	fixture.Hub.Switch.offline.Store(false)
	localFloor := fixture.NodeA.Server.Hub().Generation()
	require.Eventually(func() bool {
		fixture.Hub.Server.Hub().Broadcast(server.Event{
			Type: "data_changed", Data: map[string]string{"source": "hub"},
		})
		records, stale := fixture.NodeA.Server.Hub().RingSnapshotSince(localFloor)
		return !stale && slices.ContainsFunc(records, func(record server.RecordedEvent) bool {
			return record.ID > localFloor && record.Event.Type == "data_changed"
		})
	}, 8*time.Second, 200*time.Millisecond)
	assert.Len(fixture.pulls(t, fixture.NodeA), 2, "provider reads recover after reconnect")

	protocolRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		fixture.Hub.HTTP.URL+"/api/v1/pulls", nil,
	)
	require.NoError(err)
	protocolRequest.Header.Set("Authorization", "Bearer "+fixture.nodeBToken)
	protocolRequest.Header.Set(federationauth.NodeIDHeader, federatedNodeBID)
	protocolRequest.Header.Set(providerplane.ProtocolVersionHeader, "999")
	protocolResponse, err := fixture.HTTPClient.Do(protocolRequest)
	require.NoError(err)
	protocolResponse.Body.Close()
	assert.Equal(http.StatusConflict, protocolResponse.StatusCode)

	require.NoError(fixture.Hub.Credentials.RevokeInbound(fixture.nodeAToken))
	revokedRequest, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		fixture.Hub.HTTP.URL+"/api/v1/pulls", nil,
	)
	require.NoError(err)
	revokedRequest.Header.Set("Authorization", "Bearer "+fixture.nodeAToken)
	revokedRequest.Header.Set(federationauth.NodeIDHeader, federatedNodeAID)
	revokedRequest.Header.Set(
		providerplane.ProtocolVersionHeader,
		providerplane.ProtocolVersionHeaderValue(),
	)
	revokedResponse, err := fixture.HTTPClient.Do(revokedRequest)
	require.NoError(err)
	revokedResponse.Body.Close()
	assert.Equal(http.StatusUnauthorized, revokedResponse.StatusCode)
}

func mcpRepositoryIdentity(item mcpserver.ItemIdentity) mcpserver.RepositoryIdentity {
	return mcpserver.RepositoryIdentity{
		Provider: item.Provider, PlatformHost: item.PlatformHost,
		PlatformRepoID: item.PlatformRepoID, Owner: item.Owner, Name: item.Name,
	}
}
