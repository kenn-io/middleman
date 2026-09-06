package fleetapi

import (
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFleetTestAPI builds an in-process huma.API the same way the
// route-metadata walker tests in this package do, so registration can be
// introspected via api.OpenAPI() without standing up a real server.
func newFleetTestAPI() huma.API {
	mux := http.NewServeMux()
	return humago.New(mux, huma.DefaultConfig("test", "0.0.0"))
}

func TestSnapshotRoutesRegistered(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := &Handler{} // registration does not call handlers
	api := newFleetTestAPI()
	s.Register(api)

	spec := api.OpenAPI()
	operations := map[string]func(*huma.PathItem) *huma.Operation{
		http.MethodGet:    func(item *huma.PathItem) *huma.Operation { return item.Get },
		http.MethodPost:   func(item *huma.PathItem) *huma.Operation { return item.Post },
		http.MethodPut:    func(item *huma.PathItem) *huma.Operation { return item.Put },
		http.MethodDelete: func(item *huma.PathItem) *huma.Operation { return item.Delete },
	}
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/snapshot"},
		{http.MethodGet, "/snapshot/raw"},
		{http.MethodPost, "/fleet/hosts/{host_key}/workspaces"},
		{http.MethodPost, "/fleet/hosts/{host_key}/terminal/paste-image"},
		{http.MethodPost, "/fleet/hosts/{host_key}/repo/{provider}/{owner}/{name}/workspaces"},
		{http.MethodPost, "/fleet/hosts/{host_key}/host/{platform_host}/repo/{provider}/{owner}/{name}/workspaces"},
		{http.MethodPost, "/fleet/hosts/{host_key}/issues/{provider}/{owner}/{name}/{number}/workspace"},
		{http.MethodPost, "/fleet/hosts/{host_key}/host/{platform_host}/issues/{provider}/{owner}/{name}/{number}/workspace"},
		{http.MethodPost, "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions"},
		{http.MethodDelete, "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}"},
		{http.MethodGet, "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}/attach-spec"},
		{http.MethodGet, "/fleet/hosts/{host_key}/workspaces/{id}/commits"},
		{http.MethodGet, "/fleet/hosts/{host_key}/workspaces/{id}/diff"},
		{http.MethodGet, "/fleet/hosts/{host_key}/workspaces/{id}/file-preview"},
		{http.MethodGet, "/fleet/hosts/{host_key}/workspaces/{id}/files"},
		{http.MethodGet, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/shell"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions"},
		{http.MethodDelete, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}"},
		{http.MethodGet, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}/attach-spec"},
		{http.MethodDelete, "/fleet/hosts/{host_key}/workspaces/{id}"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects"},
		{http.MethodGet, "/fleet/hosts/{host_key}/projects/{project_id}"},
		{http.MethodDelete, "/fleet/hosts/{host_key}/projects/{project_id}"},
		{http.MethodGet, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/from-merge-request"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/delete"},
		{http.MethodPut, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/session-backend"},
		{http.MethodPut, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/linked-issues"},
		{http.MethodPost, "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/refresh-stats"},
	} {
		item := spec.Paths[route.path]
		require.NotNil(item, "%s not registered", route.path)
		require.NotNil(operations[route.method](item), "%s %s not registered", route.method, route.path)
	}

	for _, path := range []string{
		"/fleet/hosts/{host_key}/workspaces/{id}/diff",
		"/fleet/hosts/{host_key}/workspaces/{id}/file-preview",
	} {
		queryParams := make([]string, 0)
		for _, param := range spec.Paths[path].Get.Parameters {
			if param.In == "query" {
				queryParams = append(queryParams, param.Name)
			}
		}
		assert.Contains(queryParams, "revision", path)
	}

	gotProjectRegister := spec.Paths["/fleet/hosts/{host_key}/projects"].Post
	assert.Equal("register-fleet-project", gotProjectRegister.OperationID)
	assert.Equal([]string{"Fleet"}, gotProjectRegister.Tags)

	// Operation IDs + Fleet tag present on the enriched snapshot route.
	got := spec.Paths["/snapshot"].Get
	assert.Equal("get-snapshot", got.OperationID)
	assert.Equal([]string{"Fleet"}, got.Tags)

	// Operation IDs + Fleet tag present on the raw snapshot route.
	gotRaw := spec.Paths["/snapshot/raw"].Get
	assert.Equal("get-snapshot-raw", gotRaw.OperationID)
	assert.Equal([]string{"Fleet"}, gotRaw.Tags)

	gotCreate := spec.Paths["/fleet/hosts/{host_key}/workspaces"].Post
	assert.Equal("create-fleet-workspace", gotCreate.OperationID)
	assert.Equal([]string{"Fleet"}, gotCreate.Tags)
}
