package fleetapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/terminalpaste"
	"go.kenn.io/forge/internal/terminalwebsocket"
	"go.kenn.io/forge/internal/tracing"
)

type fleetRESTProxyRoute struct {
	operationID  string
	method       string
	path         string
	summary      string
	pathParams   []string
	queryParams  []*huma.Param
	body         bool
	binaryBody   bool
	maxBodyBytes int64
	targetPath   func(*http.Request) string
}

const fleetProxyMaxBodyBytes int64 = 1 << 20

type fleetHostTarget struct {
	self       bool
	member     config.FleetMember
	credential federationauth.Credential
	clients    federationMemberClients
}

type federationMemberClients struct {
	rest      *http.Client
	proxy     *http.Client
	websocket *http.Client
}

func (s *Handler) registerFleetOperationRoutes(api huma.API) {
	routes := []fleetRESTProxyRoute{
		{
			operationID:  "store-fleet-terminal-paste-image",
			method:       http.MethodPost,
			path:         "/fleet/hosts/{host_key}/terminal/paste-image",
			summary:      "Store a browser clipboard image on a fleet host",
			pathParams:   []string{"host_key"},
			binaryBody:   true,
			maxBodyBytes: terminalpaste.MaxImageBytes,
			targetPath: func(*http.Request) string {
				return "/api/v1/terminal/paste-image"
			},
		},
		{
			operationID: "list-fleet-workspaces",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces",
			summary:     "List workspaces on fleet host",
			pathParams:  []string{"host_key"},
			targetPath: func(*http.Request) string {
				return "/api/v1/workspaces"
			},
		},
		{
			operationID: "create-fleet-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces",
			summary:     "Create workspace on fleet host",
			pathParams:  []string{"host_key"},
			body:        true,
			targetPath: func(*http.Request) string {
				return "/api/v1/workspaces"
			},
		},
		{
			operationID: "create-fleet-repo-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/repo/{provider}/{owner}/{name}/workspaces",
			summary:     "Create repository workspace on fleet host",
			pathParams:  []string{"host_key", "provider", "owner", "name"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/repo/" +
					escapePath(r.PathValue("provider")) + "/" +
					escapePath(r.PathValue("owner")) + "/" +
					escapePath(r.PathValue("name")) + "/workspaces"
			},
		},
		{
			operationID: "create-fleet-repo-workspace-on-platform-host",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/host/{platform_host}/repo/{provider}/{owner}/{name}/workspaces",
			summary:     "Create repository workspace on fleet host",
			pathParams:  []string{"host_key", "platform_host", "provider", "owner", "name"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/host/" + escapePath(r.PathValue("platform_host")) +
					"/repo/" + escapePath(r.PathValue("provider")) + "/" +
					escapePath(r.PathValue("owner")) + "/" +
					escapePath(r.PathValue("name")) + "/workspaces"
			},
		},
		{
			operationID: "create-fleet-issue-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/issues/{provider}/{owner}/{name}/{number}/workspace",
			summary:     "Create issue workspace on fleet host",
			pathParams:  []string{"host_key", "provider", "owner", "name", "number"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/issues/" +
					escapePath(r.PathValue("provider")) + "/" +
					escapePath(r.PathValue("owner")) + "/" +
					escapePath(r.PathValue("name")) + "/" +
					escapePath(r.PathValue("number")) + "/workspace"
			},
		},
		{
			operationID: "create-fleet-issue-workspace-on-platform-host",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/host/{platform_host}/issues/{provider}/{owner}/{name}/{number}/workspace",
			summary:     "Create issue workspace on fleet host",
			pathParams:  []string{"host_key", "platform_host", "provider", "owner", "name", "number"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/host/" + escapePath(r.PathValue("platform_host")) +
					"/issues/" + escapePath(r.PathValue("provider")) + "/" +
					escapePath(r.PathValue("owner")) + "/" +
					escapePath(r.PathValue("name")) + "/" +
					escapePath(r.PathValue("number")) + "/workspace"
			},
		},
		{
			operationID: "get-fleet-workspace",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}",
			summary:     "Get workspace on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id"))
			},
		},
		{
			operationID: "retry-fleet-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/retry",
			summary:     "Retry workspace setup on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/retry"
			},
		},
		{
			operationID: "refresh-fleet-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/refresh",
			summary:     "Refresh workspace metadata on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/refresh"
			},
		},
		{
			operationID: "push-fleet-workspace-branch",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/push",
			summary:     "Push workspace branch on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/push"
			},
		},
		{
			operationID: "pull-fleet-workspace-branch",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/pull",
			summary:     "Pull workspace branch on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/pull"
			},
		},
		{
			operationID: "reveal-fleet-workspace",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/reveal",
			summary:     "Reveal workspace folder on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/reveal"
			},
		},
		{
			operationID: "get-fleet-workspace-runtime",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime",
			summary:     "Get workspace runtime on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/runtime"
			},
		},
		{
			operationID: "get-fleet-workspace-commits",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/commits",
			summary:     "Get workspace commits on fleet host",
			pathParams:  []string{"host_key", "id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/commits"
			},
		},
		{
			operationID: "get-fleet-workspace-diff",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/diff",
			summary:     "Get workspace diff on fleet host",
			pathParams:  []string{"host_key", "id"},
			queryParams: fleetWorkspaceDiffQueryParams(),
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/diff"
			},
		},
		{
			operationID: "get-fleet-workspace-file-preview",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/file-preview",
			summary:     "Get workspace file preview on fleet host",
			pathParams:  []string{"host_key", "id"},
			queryParams: fleetWorkspaceFilePreviewQueryParams(),
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/file-preview"
			},
		},
		{
			operationID: "get-fleet-workspace-files",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/files",
			summary:     "Get workspace files on fleet host",
			pathParams:  []string{"host_key", "id"},
			queryParams: fleetWorkspaceDiffScopeQueryParams(),
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/files"
			},
		},
		{
			operationID: "watch-fleet-workspace-diff",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/diff/watch",
			summary:     "Watch selected workspace diff on fleet host",
			pathParams:  []string{"host_key", "id"},
			queryParams: []*huma.Param{
				fleetStringQueryParam("version", "Last observed workspace diff snapshot version."),
			},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/diff/watch"
			},
		},
		{
			operationID: "launch-fleet-workspace-runtime-session",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions",
			summary:     "Launch workspace session on fleet host",
			pathParams:  []string{"host_key", "id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) + "/runtime/sessions"
			},
		},
		{
			operationID: "rename-fleet-workspace-runtime-session",
			method:      http.MethodPatch,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}",
			summary:     "Rename workspace session on fleet host",
			pathParams:  []string{"host_key", "id", "session_key"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key"))
			},
		},
		{
			operationID: "stop-fleet-workspace-runtime-session",
			method:      http.MethodDelete,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}",
			summary:     "Stop workspace session on fleet host",
			pathParams:  []string{"host_key", "id", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key"))
			},
		},
		{
			operationID: "get-fleet-workspace-runtime-session-attach-spec",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}/attach-spec",
			summary:     "Get workspace session attach spec on fleet host",
			pathParams:  []string{"host_key", "id", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key")) +
					"/attach-spec"
			},
		},
		{
			operationID: "complete-fleet-filesystem-path",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/filesystem/complete",
			summary:     "Complete a filesystem path on fleet host",
			pathParams:  []string{"host_key"},
			queryParams: []*huma.Param{fleetFilesystemPathQueryParam(
				"The partial path to complete on the owning host.",
			)},
			targetPath: func(r *http.Request) string {
				return "/api/v1/filesystem/complete"
			},
		},
		{
			operationID: "validate-fleet-filesystem-repo",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/filesystem/validate-repo",
			summary:     "Resolve a repository root on fleet host",
			pathParams:  []string{"host_key"},
			queryParams: []*huma.Param{fleetFilesystemPathQueryParam(
				"The path to resolve to a repository root on the owning host.",
			)},
			targetPath: func(r *http.Request) string {
				return "/api/v1/filesystem/validate-repo"
			},
		},
		{
			operationID: "launch-fleet-host-runtime-session",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/runtime/sessions",
			summary:     "Launch host runtime session on fleet host",
			pathParams:  []string{"host_key"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/runtime/sessions"
			},
		},
		{
			operationID: "stop-fleet-host-runtime-session",
			method:      http.MethodDelete,
			path:        "/fleet/hosts/{host_key}/runtime/sessions/{session_key}",
			summary:     "Stop host runtime session on fleet host",
			pathParams:  []string{"host_key", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/runtime/sessions/" +
					escapePath(r.PathValue("session_key"))
			},
		},
		{
			operationID: "get-fleet-host-runtime-session-attach-spec",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/runtime/sessions/{session_key}/attach-spec",
			summary:     "Get host runtime session attach spec on fleet host",
			pathParams:  []string{"host_key", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/runtime/sessions/" +
					escapePath(r.PathValue("session_key")) +
					"/attach-spec"
			},
		},
		{
			operationID: "clone-fleet-project",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/clone",
			summary:     "Clone a repository into a project on fleet host",
			pathParams:  []string{"host_key"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/clone"
			},
		},
		{
			operationID: "list-fleet-project-branches",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/branches",
			summary:     "List project branches on fleet host",
			pathParams:  []string{"host_key", "project_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/branches"
			},
		},
		{
			operationID: "get-fleet-project",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}",
			summary:     "Get project on fleet host",
			pathParams:  []string{"host_key", "project_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id"))
			},
		},
		{
			operationID: "list-fleet-project-worktrees",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees",
			summary:     "List project worktrees on fleet host",
			pathParams:  []string{"host_key", "project_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees"
			},
		},
		{
			operationID: "inspect-fleet-project-worktree",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/inspect",
			summary:     "Inspect project worktree on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/inspect"
			},
		},
		{
			operationID: "get-fleet-project-worktree-runtime",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime",
			summary:     "Get project worktree runtime on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/runtime"
			},
		},
		{
			operationID: "ensure-fleet-project-worktree-runtime-shell",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/shell",
			summary:     "Ensure project worktree shell on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/runtime/shell"
			},
		},
		{
			operationID: "launch-fleet-project-worktree-runtime-session",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions",
			summary:     "Launch project worktree session on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/runtime/sessions"
			},
		},
		{
			operationID: "stop-fleet-project-worktree-runtime-session",
			method:      http.MethodDelete,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}",
			summary:     "Stop project worktree session on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key"))
			},
		},
		{
			operationID: "get-fleet-project-worktree-runtime-session-attach-spec",
			method:      http.MethodGet,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}/attach-spec",
			summary:     "Get project worktree session attach spec on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id", "session_key"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key")) +
					"/attach-spec"
			},
		},
		{
			operationID: "delete-fleet-workspace",
			method:      http.MethodDelete,
			path:        "/fleet/hosts/{host_key}/workspaces/{id}",
			summary:     "Delete workspace on fleet host",
			pathParams:  []string{"host_key", "id"},
			queryParams: []*huma.Param{fleetForceQueryParam()},
			targetPath: func(r *http.Request) string {
				return "/api/v1/workspaces/" + escapePath(r.PathValue("id"))
			},
		},
		{
			operationID: "create-fleet-project-worktree",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees",
			summary:     "Create project worktree on fleet host",
			pathParams:  []string{"host_key", "project_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees"
			},
		},
		{
			operationID: "create-fleet-project-worktree-from-merge-request",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/from-merge-request",
			summary:     "Create project worktree from a merge request on fleet host",
			pathParams:  []string{"host_key", "project_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/from-merge-request"
			},
		},
		{
			operationID: "remove-fleet-project-worktree",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/delete",
			summary:     "Remove project worktree on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/delete"
			},
		},
		{
			operationID: "set-fleet-project-worktree-session-backend",
			method:      http.MethodPut,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/session-backend",
			summary:     "Set project worktree session backend on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/session-backend"
			},
		},
		{
			// "links" (not "linked-issues") to match the local op id's
			// generic-registry naming; the path keeps the precise field name.
			operationID: "set-fleet-project-worktree-links",
			method:      http.MethodPut,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/linked-issues",
			summary:     "Set project worktree linked issues on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			body:        true,
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/linked-issues"
			},
		},
		{
			operationID: "refresh-fleet-project-worktree-stats",
			method:      http.MethodPost,
			path:        "/fleet/hosts/{host_key}/projects/{project_id}/worktrees/{worktree_id}/refresh-stats",
			summary:     "Refresh project worktree git stats on fleet host",
			pathParams:  []string{"host_key", "project_id", "worktree_id"},
			targetPath: func(r *http.Request) string {
				return "/api/v1/projects/" + escapePath(r.PathValue("project_id")) +
					"/worktrees/" + escapePath(r.PathValue("worktree_id")) +
					"/refresh-stats"
			},
		},
	}

	for _, route := range routes {
		maxBodyBytes := route.maxBodyBytes
		if maxBodyBytes == 0 {
			maxBodyBytes = fleetProxyMaxBodyBytes
		}
		op := &huma.Operation{
			OperationID:  route.operationID,
			Method:       route.method,
			Path:         route.path,
			Summary:      route.summary,
			Tags:         []string{"Fleet"},
			Parameters:   fleetProxyParams(route.pathParams, route.queryParams...),
			Responses:    fleetProxyResponses(),
			MaxBodyBytes: maxBodyBytes,
		}
		if route.binaryBody {
			op.RequestBody = fleetProxyBinaryRequestBody()
		} else if route.body {
			op.RequestBody = fleetProxyRequestBody()
		}
		api.OpenAPI().AddOperation(op)
		api.Adapter().Handle(op, func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			if !bufferFleetProxyRequestBody(w, r, maxBodyBytes) {
				return
			}
			s.serveFleetRESTProxy(w, r, route.targetPath(r))
		})
	}
}

// RegisterTerminal registers hidden Fleet websocket proxy routes.
func (s *Handler) RegisterTerminal(api huma.API) {
	routes := []struct {
		operationID string
		path        string
		targetPath  func(*http.Request) string
	}{
		{
			operationID: "connect-fleet-workspace-terminal",
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/terminal",
			targetPath: func(r *http.Request) string {
				return "/ws/v1/workspaces/" + escapePath(r.PathValue("id")) + "/terminal"
			},
		},
		{
			operationID: "connect-fleet-workspace-runtime-session-terminal",
			path:        "/fleet/hosts/{host_key}/workspaces/{id}/runtime/sessions/{session_key}/terminal",
			targetPath: func(r *http.Request) string {
				return "/ws/v1/workspaces/" + escapePath(r.PathValue("id")) +
					"/runtime/sessions/" + escapePath(r.PathValue("session_key")) + "/terminal"
			},
		},
	}

	for _, route := range routes {
		op := &huma.Operation{
			OperationID: route.operationID,
			Method:      http.MethodGet,
			Path:        route.path,
			Hidden:      true,
		}
		api.Adapter().Handle(op, func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			s.serveFleetWebSocketProxy(w, r, route.targetPath(r))
		})
	}
}

func fleetProxyParams(pathParams []string, queryParams ...*huma.Param) []*huma.Param {
	params := make([]*huma.Param, 0, len(pathParams)+len(queryParams))
	for _, name := range pathParams {
		params = append(params, &huma.Param{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   fleetStringSchema(),
		})
	}
	params = append(params, queryParams...)
	return params
}

func fleetFilesystemPathQueryParam(description string) *huma.Param {
	return &huma.Param{
		Name:        "path",
		In:          "query",
		Description: description,
		Required:    true,
		Schema:      fleetStringSchema(),
	}
}

func fleetForceQueryParam() *huma.Param {
	return &huma.Param{
		Name:        "force",
		In:          "query",
		Description: "Forward force deletion to the owning host.",
		Schema: &huma.Schema{
			Type: "boolean",
		},
	}
}

func fleetWorkspaceDiffQueryParams() []*huma.Param {
	return append(
		fleetWorkspaceDiffScopeQueryParams(),
		fleetStringQueryParam("revision", "Pinned workspace snapshot version."),
	)
}

func fleetWorkspaceDiffScopeQueryParams() []*huma.Param {
	return []*huma.Param{
		fleetStringQueryParam("base", "Workspace diff base."),
		fleetStringQueryParam("whitespace", "Whitespace filtering mode."),
		fleetStringQueryParam("commit", "Commit SHA scope."),
		fleetStringQueryParam("from", "Older range commit SHA."),
		fleetStringQueryParam("to", "Newer range commit SHA."),
	}
}

func fleetWorkspaceFilePreviewQueryParams() []*huma.Param {
	return append(
		fleetWorkspaceDiffQueryParams(),
		fleetStringQueryParam("path", "Workspace file path to preview."),
		fleetStringQueryParam("side", "Preview side."),
	)
}

func fleetStringQueryParam(name, description string) *huma.Param {
	return &huma.Param{
		Name:        name,
		In:          "query",
		Description: description,
		Schema:      fleetStringSchema(),
	}
}

func fleetStringSchema() *huma.Schema {
	return &huma.Schema{Type: "string"}
}

func fleetProxyRequestBody() *huma.RequestBody {
	return &huma.RequestBody{
		Description: "JSON payload forwarded to the owning host.",
		Required:    true,
		Content: map[string]*huma.MediaType{
			"application/json": {
				Schema: &huma.Schema{
					Type:                 "object",
					AdditionalProperties: true,
				},
			},
		},
	}
}

// bufferFleetProxyRequestBody bounds and consumes any browser request body
// before the hub resolves or dials a fleet member. The fleet adapter handles
// raw requests, so Huma's MaxBodyBytes metadata is not enforced automatically.
func bufferFleetProxyRequestBody(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) bool {
	if r.ContentLength > maxBodyBytes {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusRequestEntityTooLarge,
			httpapi.CodePayloadTooLarge,
			"fleet proxy request body is too large",
			map[string]any{"maxBytes": maxBodyBytes},
		))
		return false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusBadRequest,
			httpapi.CodeBadRequest,
			"could not read fleet proxy request body",
			nil,
		))
		return false
	}
	if int64(len(body)) > maxBodyBytes {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusRequestEntityTooLarge,
			httpapi.CodePayloadTooLarge,
			"fleet proxy request body is too large",
			map[string]any{"maxBytes": maxBodyBytes},
		))
		return false
	}

	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

func fleetProxyBinaryRequestBody() *huma.RequestBody {
	return &huma.RequestBody{
		Description: "Browser clipboard image forwarded to the owning host.",
		Required:    true,
		Content: map[string]*huma.MediaType{
			"application/octet-stream": {
				Schema: &huma.Schema{Type: "string", Format: "binary"},
			},
		},
	}
}

func fleetProxyResponses() map[string]*huma.Response {
	return map[string]*huma.Response{
		"default": {
			Description: "Response returned by the owning fleet host.",
			Content: map[string]*huma.MediaType{
				"application/json": {
					Schema: &huma.Schema{
						Type:                 "object",
						AdditionalProperties: true,
					},
				},
				"application/problem+json": {
					Schema: &huma.Schema{
						Ref: "#/components/schemas/ProblemError",
					},
				},
			},
		},
	}
}

func (s *Handler) serveFleetRESTProxy(
	w http.ResponseWriter,
	r *http.Request,
	targetPath string,
) {
	target, ok := s.resolveFleetHostTarget(r.PathValue("host_key"))
	if !ok {
		writeProblemResponse(w, fleetHostNotFoundProblem(r.PathValue("host_key")))
		return
	}
	if target.self {
		s.serveLocalFleetRESTProxy(w, r, targetPath)
		return
	}
	s.serveRemoteFleetRESTProxy(w, r, target, targetPath)
}

func (s *Handler) serveLocalFleetRESTProxy(
	w http.ResponseWriter,
	r *http.Request,
	targetPath string,
) {
	if s.localHandler == nil || s.localHandler() == nil {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusServiceUnavailable,
			httpapi.CodeServiceUnavailable,
			"server handler not configured",
			nil,
		))
		return
	}
	proxyReq := cloneRequestForLocalPath(r, s.localProxyPath(targetPath))
	s.localHandler().ServeHTTP(w, proxyReq)
}

func (s *Handler) serveRemoteFleetRESTProxy(
	w http.ResponseWriter,
	r *http.Request,
	target fleetHostTarget,
	targetPath string,
) {
	req, err := http.NewRequestWithContext(
		r.Context(),
		r.Method,
		remoteHTTPURL(target.member.BaseURL, targetPath, r.URL.RawQuery),
		r.Body,
	)
	if err != nil {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusBadGateway,
			httpapi.CodeUpstreamError,
			"build fleet peer request: "+err.Error(),
			map[string]any{"hostKey": target.member.NodeID},
		))
		return
	}
	copyProxyRequestHeaders(req.Header, r.Header)
	s.authorizeFederationRequest(req.Header, target.credential)

	resp, err := target.clients.proxy.Do(req)
	if err != nil {
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusBadGateway,
			httpapi.CodeUpstreamError,
			"fleet peer request failed: "+err.Error(),
			map[string]any{"hostKey": target.member.NodeID},
		))
		return
	}
	defer resp.Body.Close()

	copyProxyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug(
			"copy fleet peer response",
			"host_key", target.member.NodeID,
			"target", targetPath,
			"err", err,
		)
	}
}

func (s *Handler) serveFleetWebSocketProxy(
	w http.ResponseWriter,
	r *http.Request,
	targetPath string,
) {
	target, ok := s.resolveFleetHostTarget(r.PathValue("host_key"))
	if !ok {
		writeProblemResponse(w, fleetHostNotFoundProblem(r.PathValue("host_key")))
		return
	}
	if target.self {
		if s.localHandler == nil || s.localHandler() == nil {
			writeProblemResponse(w, httpapi.NewProblem(
				http.StatusServiceUnavailable,
				httpapi.CodeServiceUnavailable,
				"server handler not configured",
				nil,
			))
			return
		}
		proxyReq := cloneRequestForLocalPath(r, s.localProxyPath(targetPath))
		s.localHandler().ServeHTTP(w, proxyReq)
		return
	}

	r, attachSpan, endAttachSpan := startFleetAttachSpan(r)
	defer endAttachSpan()

	peerURL := remoteWebSocketURL(target.member.BaseURL, targetPath, r.URL.RawQuery)
	dialHeader := make(http.Header)
	copyProxyWebSocketRequestHeaders(dialHeader, r.Header)
	s.authorizeFederationRequest(dialHeader, target.credential)
	peerConn, _, err := terminalwebsocket.Dial(
		r.Context(), peerURL, dialHeader, target.clients.websocket,
	)
	if err != nil {
		attachSpan.SetAttributes(attribute.Bool("error", true))
		writeProblemResponse(w, httpapi.NewProblem(
			http.StatusBadGateway,
			httpapi.CodeUpstreamError,
			"fleet peer websocket failed: "+err.Error(),
			map[string]any{"hostKey": target.member.NodeID},
		))
		return
	}
	defer peerConn.Close(websocket.StatusNormalClosure, "hub detached")

	clientConn, err := terminalwebsocket.Accept(w, r)
	if err != nil {
		attachSpan.SetAttributes(attribute.Bool("error", true))
		slog.Debug(
			"fleet websocket accept failed",
			"host_key", target.member.NodeID,
			"err", err,
		)
		return
	}
	defer clientConn.Close(websocket.StatusNormalClosure, "hub detached")

	endAttachSpan()
	bridgeWebSocketProxy(r.Context(), clientConn, peerConn)
}

func startFleetAttachSpan(r *http.Request) (*http.Request, trace.Span, func()) {
	ctx, attachSpan := tracing.StartAttachSpan(r, "terminal.attach")
	endAttachSpan := sync.OnceFunc(func() { attachSpan.End() })
	return r.WithContext(ctx), attachSpan, endAttachSpan
}

func bridgeWebSocketProxy(ctx context.Context, client, peer *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go proxyWebSocketMessages(ctx, &wg, client, peer, cancel)
	go proxyWebSocketMessages(ctx, &wg, peer, client, cancel)
	wg.Wait()
}

func proxyWebSocketMessages(
	ctx context.Context,
	wg *sync.WaitGroup,
	from *websocket.Conn,
	to *websocket.Conn,
	cancel context.CancelFunc,
) {
	defer wg.Done()
	defer cancel()
	for {
		typ, data, err := from.Read(ctx)
		if err != nil {
			return
		}
		if err := to.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// fleetSelfHostAlias always addresses the local host without exposing callers
// to the daemon's stable node ID.
const fleetSelfHostAlias = "self"

func (s *Handler) resolveFleetHostTarget(hostKey string) (fleetHostTarget, bool) {
	hostKey = strings.TrimSpace(hostKey)
	if hostKey == "" {
		return fleetHostTarget{}, false
	}
	if hostKey == fleetSelfHostAlias || hostKey == s.fleetSelfKey("") {
		return fleetHostTarget{self: true}, true
	}
	fleetCfg := s.configSnapshot().Fleet
	if fleetCfg.Enabled && fleetCfg.RoleOrDefault() == config.FleetRoleHub {
		for _, member := range fleetCfg.Members {
			if member.NodeID == hostKey {
				return s.resolveEnrolledSpoke(member)
			}
		}
	}
	return fleetHostTarget{}, false
}

func (s *Handler) fleetSelfKey(localHostname string) string {
	if federation.ValidNodeID(s.nodeID) {
		return s.nodeID
	}
	// Production startup always supplies the persisted node ID. The hostname
	// fallback keeps isolated package fixtures useful without inventing identity.
	if strings.TrimSpace(localHostname) != "" {
		return strings.TrimSpace(localHostname)
	}
	return hostnameOrEmpty()
}

func (s *Handler) resolveEnrolledSpoke(
	member config.FleetMember,
) (fleetHostTarget, bool) {
	enrollment, ok := s.enrollments.EnrollmentForSpoke(member.NodeID)
	if !ok || enrollment.State != federation.EnrollmentActive ||
		enrollment.ActivationLeaseVersion != federation.ActivationLeaseVersion ||
		!enrollment.ActivationValidUntil.After(s.now().UTC()) ||
		enrollment.SpokeBaseURL != member.BaseURL {
		return fleetHostTarget{}, false
	}
	member.BaseURL = enrollment.SpokeBaseURL
	return s.resolveEnrolledMember(member)
}

func (s *Handler) resolveEnrolledMember(
	member config.FleetMember,
) (fleetHostTarget, bool) {
	if member.State != federation.EnrollmentActive || s.credentials == nil {
		return fleetHostTarget{}, false
	}
	credential, ok := s.credentials.Outbound(member.NodeID)
	if !ok {
		return fleetHostTarget{}, false
	}
	return fleetHostTarget{
		member: member, credential: credential,
		clients: s.memberClientsForOrigin(member.BaseURL),
	}, true
}

func (s *Handler) memberClientsForOrigin(origin string) federationMemberClients {
	s.memberClientsMu.Lock()
	defer s.memberClientsMu.Unlock()
	if clients, ok := s.memberClients[origin]; ok {
		return clients
	}
	clients := newFederationMemberClients(s.federationHTTPClient)
	s.memberClients[origin] = clients
	return clients
}

func (s *Handler) authorizeFederationRequest(
	header http.Header, credential federationauth.Credential,
) {
	header.Set("Authorization", "Bearer "+credential.Token)
	if federation.ValidNodeID(s.nodeID) {
		header.Set(federationauth.NodeIDHeader, s.nodeID)
	}
}

func (s *Handler) localProxyPath(targetPath string) string {
	if s.basePath == "" || s.basePath == "/" {
		return targetPath
	}
	return strings.TrimSuffix(s.basePath, "/") + targetPath
}

func cloneRequestForLocalPath(r *http.Request, targetPath string) *http.Request {
	proxyReq := r.Clone(r.Context())
	u := *r.URL
	if path, err := url.PathUnescape(targetPath); err == nil {
		u.Path = path
		if path == targetPath {
			u.RawPath = ""
		} else {
			u.RawPath = targetPath
		}
	} else {
		u.Path = targetPath
		u.RawPath = ""
	}
	proxyReq.URL = &u
	proxyReq.RequestURI = ""
	return proxyReq
}

func remoteHTTPURL(baseURL, targetPath, rawQuery string) string {
	return strings.TrimRight(baseURL, "/") + targetPath + querySuffix(rawQuery)
}

func remoteWebSocketURL(baseURL, targetPath, rawQuery string) string {
	u, err := url.Parse(remoteHTTPURL(baseURL, targetPath, rawQuery))
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	return u.String()
}

func querySuffix(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	return "?" + rawQuery
}

func escapePath(value string) string {
	return url.PathEscape(value)
}

func copyProxyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) || isPeerProxyClientHeader(key) ||
			isPeerProxyCredentialHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyProxyWebSocketRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || isPeerProxyClientHeader(key) ||
			isPeerProxyCredentialHeader(key) ||
			strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// isPeerProxyCredentialHeader reports whether key carries the caller's
// credentials, which must never ride along on a server-to-server fleet proxy
// request. The browser bearer and cookie authenticate the local daemon; the
// proxy adds only the enrolled destination credential after resolving a member.
func isPeerProxyCredentialHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie":
		return true
	default:
		return false
	}
}

// isPeerProxyClientHeader reports whether key is client/proxy metadata that
// must not ride along on a server-to-server fleet proxy request. The hub fans
// out on behalf of a browser and may sit behind a reverse proxy, so inbound
// Origin, Sec-Fetch-*, Forwarded, and X-Forwarded-* metadata describe the hub
// request, not the peer request. Peers validate those values when host-
// authority protection or trust_reverse_proxy is enabled.
func isPeerProxyClientHeader(key string) bool {
	lower := strings.ToLower(key)
	return lower == "origin" ||
		lower == "forwarded" ||
		strings.HasPrefix(lower, "sec-fetch-") ||
		strings.HasPrefix(lower, "x-forwarded-")
}

func copyProxyResponseHeaders(dst, src http.Header) {
	connectionHeaders := proxyConnectionTokens(src)
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(lower) || connectionHeaders[lower] ||
			isUnsafePeerResponseHeader(lower) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func proxyConnectionTokens(header http.Header) map[string]bool {
	tokens := make(map[string]bool)
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token != "" {
				tokens[token] = true
			}
		}
	}
	return tokens
}

func isUnsafePeerResponseHeader(lower string) bool {
	switch lower {
	case "set-cookie", "location", "clear-site-data", "www-authenticate":
		return true
	default:
		return false
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection",
		"keep-alive",
		"proxy-authenticate",
		"proxy-authorization",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade":
		return true
	default:
		return false
	}
}

func fleetHostNotFoundProblem(hostKey string) *httpapi.ProblemError {
	return httpapi.NewProblem(
		http.StatusNotFound,
		httpapi.CodeNotFound,
		"fleet host not found",
		map[string]any{"hostKey": hostKey},
	)
}

func writeProblemResponse(w http.ResponseWriter, problem *httpapi.ProblemError) {
	if problem == nil {
		problem = httpapi.NewProblem(
			http.StatusInternalServerError,
			httpapi.CodeInternalError,
			"internal error",
			nil,
		)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(problem)
}
