package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil"
)

func TestArchiveStartRejectsDisabledSyncer(t *testing.T) {
	require := require.New(t)
	database := openTestDB(t)
	syncer := github.NewSyncer(nil, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	syncer.DisableSync()
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})

	rr := testutil.DoJSON(t, srv, http.MethodPost, "/api/v1/archive/start", map[string]bool{"all": true})
	require.Equal(http.StatusServiceUnavailable, rr.Code, rr.Body.String())
}
