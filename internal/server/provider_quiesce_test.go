package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func authenticatedProviderRequest(
	t *testing.T, server *httptest.Server, method, path string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(), method, server.URL+path, bytes.NewReader([]byte(`{"ids":[1]}`)),
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer local-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestStandaloneProviderWritesDoNotRequireSpokePreparationState(t *testing.T) {
	database := dbtest.Open(t)
	_, err := database.WriteDB().ExecContext(t.Context(), "DROP TABLE forge_spoke_preparation")
	require.NoError(t, err)

	srv := New(database, nil, nil, "/", nil, ServerOptions{})
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/github/acme/widget/1/comments",
		bytes.NewReader([]byte(`{"body":""}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	require.NotContains(t, rr.Body.String(), "spoke preparation")
}

func TestLocalEnrollmentRestoresSpokePreparationBarrier(t *testing.T) {
	database := dbtest.Open(t)
	gate := providerplane.NewProviderWriteGate(database, true)
	_, err := gate.BeginQuiesce(t.Context(), db.SpokePreparationBinding{
		EnrollmentID: preparationEnrollmentID, HubNodeID: preparationHubNodeID,
		LocalNodeID: preparationLocalNodeID, ProtocolVersion: federation.ProtocolVersion,
	})
	require.NoError(t, err)

	enrollments, _ := openFederationPreparationStores(t, "restore-barrier")
	require.NoError(t, enrollments.SaveLocal(t.Context(), federation.LocalEnrollment{
		EnrollmentID: preparationEnrollmentID, NodeID: preparationLocalNodeID,
		SpokeBaseURL: "https://spoke.example", HubID: preparationHubNodeID,
		HubURL: "https://hub.example", ProtocolVersion: federation.ProtocolVersion,
		State: federation.EnrollmentPending, ExpiresAt: time.Now().Add(time.Hour),
	}))

	srv := New(database, nil, nil, "/", nil, ServerOptions{
		DaemonAccess:          DaemonAccessOptions{Token: "local-secret", RequireAPIAuth: true},
		FederationEnrollments: enrollments,
	})
	daemon := httptest.NewServer(srv)
	t.Cleanup(daemon.Close)
	blocked := authenticatedProviderRequest(
		t, daemon, http.MethodPost, "/api/v1/notifications/read",
	)

	require.Equal(t, http.StatusConflict, blocked.StatusCode)
}

func TestSpokePreparationBarrierGatesAuthenticatedProviderWritesAndSurvivesRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	gate := providerplane.NewProviderWriteGate(database, true)
	releaseWrite, err := gate.Admit(t.Context())
	require.NoError(err)
	releaseDeferred, err := gate.BeginDeferredMerge(t.Context())
	require.NoError(err)
	_, err = gate.BeginQuiesce(t.Context(), db.SpokePreparationBinding{
		EnrollmentID: "enrollment-1", HubNodeID: "hub-1",
		LocalNodeID: "spoke-1", ProtocolVersion: 3,
	})
	require.NoError(err)

	newDaemon := func(writeGate *providerplane.ProviderWriteGate) *httptest.Server {
		srv := New(database, nil, nil, "/", nil, ServerOptions{
			DaemonAccess:      DaemonAccessOptions{Token: "local-secret", RequireAPIAuth: true},
			ProviderWriteGate: writeGate,
		})
		ts := httptest.NewServer(srv)
		t.Cleanup(ts.Close)
		return ts
	}
	first := newDaemon(gate)
	blocked := authenticatedProviderRequest(
		t, first, http.MethodPost, "/api/v1/notifications/read",
	)
	require.Equal(http.StatusConflict, blocked.StatusCode)
	var problem httpapi.ProblemError
	require.NoError(json.NewDecoder(blocked.Body).Decode(&problem))
	assert.Equal(httpapi.CodeSpokePreparationInProgress, problem.Code)
	assert.Equal("spokePreparationInProgress", problem.Details["reason"])

	read := authenticatedProviderRequest(t, first, http.MethodGet, "/api/v1/version")
	assert.Equal(http.StatusOK, read.StatusCode)
	status, err := gate.Status(t.Context())
	require.NoError(err)
	assert.Equal(1, status.InFlightProviderWrites)
	assert.Equal(1, status.ActiveDeferredMerges)
	assert.Nil(status.DrainAckGeneration)

	releaseWrite()
	releaseDeferred()
	status, err = gate.Status(t.Context())
	require.NoError(err)
	assert.NotNil(status.DrainAckGeneration)

	restarted := providerplane.NewProviderWriteGate(database, true)
	second := newDaemon(restarted)
	blocked = authenticatedProviderRequest(
		t, second, http.MethodPost, "/api/v1/notifications/read",
	)
	assert.Equal(http.StatusConflict, blocked.StatusCode)
}
