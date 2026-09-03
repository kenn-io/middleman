package providerplane

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func TestSpokePreparationWriteGateSurvivesRestartAndTracksDeferredWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "gate.db")
	database := dbtest.OpenAt(t, path)
	gate := NewProviderWriteGate(database, true)

	releaseWrite, err := gate.Admit(t.Context())
	require.NoError(err)
	releaseDeferred, err := gate.BeginDeferredMerge(t.Context())
	require.NoError(err)
	_, err = gate.BeginQuiesce(t.Context(), db.SpokePreparationBinding{
		EnrollmentID: "enrollment-1", HubNodeID: "hub-1",
		LocalNodeID: "spoke-1", ProtocolVersion: 3,
	})
	require.NoError(err)
	_, err = gate.Admit(t.Context())
	require.ErrorIs(err, ErrSpokePreparationInProgress)
	require.Error(gate.CanAbortPreparation())
	status, err := gate.Status(t.Context())
	require.NoError(err)
	assert.Equal(1, status.InFlightProviderWrites)
	assert.Equal(1, status.ActiveDeferredMerges)
	releaseWrite()
	releaseDeferred()
	require.NoError(gate.CanAbortPreparation())
	status, err = gate.Status(t.Context())
	require.NoError(err)
	assert.NotNil(status.DrainAckGeneration)
	require.NoError(database.Close())

	database = dbtest.OpenPreparedAt(t, path)
	restarted := NewProviderWriteGate(database, true)
	_, err = restarted.Admit(t.Context())
	require.ErrorIs(err, ErrSpokePreparationInProgress)
	require.NoError(restarted.AbortPreparation(t.Context()))
	release, err := restarted.Admit(t.Context())
	require.NoError(err)
	release()
	require.NoError(database.Close())

	database = dbtest.OpenPreparedAt(t, path)
	afterAbortRestart := NewProviderWriteGate(database, true)
	release, err = afterAbortRestart.Admit(t.Context())
	require.NoError(err, "aborting preparation must durably reopen provider writes")
	release()
}

func TestAbortPreparationRecoversUnreadableDurableState(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	_, err := database.WriteDB().ExecContext(
		t.Context(),
		"UPDATE forge_spoke_preparation SET updated_at = 'not-a-time' WHERE singleton_id = 1",
	)
	require.NoError(err)
	gate := NewProviderWriteGate(database, true)

	require.NoError(gate.CanAbortPreparation())
	require.NoError(gate.AbortPreparation(t.Context()))
	release, err := gate.Admit(t.Context())
	require.NoError(err)
	release()
}
