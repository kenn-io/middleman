package federationauth

import (
	"context"
	"net/http"
	"strings"
)

// NodeIDHeader is optional diagnostic provenance. The bearer remains the
// authority and a supplied header must agree with its subject.
const NodeIDHeader = "X-Kenn-Forge-Node-ID"

type authorizedRoute struct {
	method string
	path   string
	scope  Scope
}

// The inventory starts intentionally narrow. Later federation domains extend
// it alongside the routes they make peer-callable; an unlisted route is denied.
var authorizedRoutes = []authorizedRoute{
	{method: http.MethodPost, path: "/api/v1/terminal/paste-image", scope: ScopeTerminalAttach},
	{method: http.MethodGet, path: "/api/v1/snapshot", scope: ScopeSnapshotRead},
	{method: http.MethodGet, path: "/api/v1/snapshot/raw", scope: ScopeSnapshotRead},
	{method: http.MethodGet, path: "/api/v1/snapshot/aggregate", scope: ScopeSnapshotRead},
	{method: http.MethodGet, path: "/api/v1/workspaces", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/workspaces", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}", scope: ScopeWorkspaceRead},
	{method: http.MethodDelete, path: "/api/v1/workspaces/{id}", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/retry", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/refresh", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/federation/workspaces/{id}/cleanup", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/push", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/pull", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/reveal", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/runtime", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/commits", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/diff", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/file-preview", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/files", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/diff/watch", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/workspaces/{id}/runtime/sessions", scope: ScopeWorkspaceWrite},
	{method: http.MethodPatch, path: "/api/v1/workspaces/{id}/runtime/sessions/{session_key}", scope: ScopeWorkspaceWrite},
	{method: http.MethodDelete, path: "/api/v1/workspaces/{id}/runtime/sessions/{session_key}", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/workspaces/{id}/runtime/sessions/{session_key}/attach-spec", scope: ScopeTerminalAttach},
	{method: http.MethodPost, path: "/api/v1/repo/{provider}/{owner}/{name}/workspaces", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/host/{platform_host}/repo/{provider}/{owner}/{name}/workspaces", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/issues/{provider}/{owner}/{name}/{number}/workspace", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/host/{platform_host}/issues/{provider}/{owner}/{name}/{number}/workspace", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/filesystem/complete", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/filesystem/validate-repo", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/runtime/sessions", scope: ScopeWorkspaceWrite},
	{method: http.MethodDelete, path: "/api/v1/runtime/sessions/{session_key}", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/runtime/sessions/{session_key}/attach-spec", scope: ScopeTerminalAttach},
	{method: http.MethodPost, path: "/api/v1/projects", scope: ScopeWorkspaceWrite},
	{method: http.MethodDelete, path: "/api/v1/projects/{project_id}", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/projects/clone", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}/branches", scope: ScopeWorkspaceRead},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}/worktrees", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees/from-merge-request", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/inspect", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/delete", scope: ScopeWorkspaceWrite},
	{method: http.MethodPut, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/session-backend", scope: ScopeWorkspaceWrite},
	{method: http.MethodPut, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/linked-issues", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/refresh-stats", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/runtime", scope: ScopeWorkspaceRead},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/runtime/shell", scope: ScopeWorkspaceWrite},
	{method: http.MethodPost, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions", scope: ScopeWorkspaceWrite},
	{method: http.MethodDelete, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}", scope: ScopeWorkspaceWrite},
	{method: http.MethodGet, path: "/api/v1/projects/{project_id}/worktrees/{worktree_id}/runtime/sessions/{session_key}/attach-spec", scope: ScopeTerminalAttach},
	{method: http.MethodGet, path: "/ws/v1/workspaces/{id}/terminal", scope: ScopeTerminalAttach},
	{method: http.MethodGet, path: "/ws/v1/workspaces/{id}/runtime/sessions/{session_key}/terminal", scope: ScopeTerminalAttach},
	{method: http.MethodGet, path: "/api/v1/federation/identity", scope: ScopeEnrollmentActivate},
	{method: http.MethodGet, path: "/api/v1/federation/events", scope: ScopeEventsRead},
	{
		method: http.MethodPost,
		path:   "/api/v1/federation/enrollments/{enrollment_id}/activate",
		scope:  ScopeEnrollmentActivate,
	},
	{
		method: http.MethodPost,
		path:   "/api/v1/federation/enrollments/{enrollment_id}/preparation/begin",
		scope:  ScopeEnrollmentActivate,
	},
	{
		method: http.MethodPost,
		path:   "/api/v1/federation/enrollments/{enrollment_id}/preparation/seal",
		scope:  ScopeEnrollmentActivate,
	},
	{
		method: http.MethodPost,
		path:   "/api/v1/federation/enrollments/{enrollment_id}/abort",
		scope:  ScopeEnrollmentActivate,
	},
	{
		method: http.MethodDelete,
		path:   "/api/v1/fleet/enrollments/{enrollment_id}",
		scope:  ScopeEnrollmentActivate,
	},
}

// Authenticator binds a credential store to the finite peer route inventory.
type Authenticator struct {
	store *Store
}

// NewAuthenticator constructs an authenticator over store.
func NewAuthenticator(store *Store) *Authenticator {
	if store == nil {
		return nil
	}
	return &Authenticator{store: store}
}

// Authenticate resolves an inbound bearer to its federation principal.
func (a *Authenticator) Authenticate(token string) (Principal, bool) {
	if a == nil {
		return Principal{}, false
	}
	return a.store.Authenticate(token)
}

// RequiredScope returns the scope assigned to an exact method/path pair.
func (a *Authenticator) RequiredScope(method, path string) (Scope, bool) {
	if a == nil {
		return "", false
	}
	return RouteScope(method, path)
}

// RouteScope returns the peer scope for one method and canonical API path.
// It exposes the closed inventory to route-ownership coverage without making
// authentication depend on a live credential store.
func RouteScope(method, path string) (Scope, bool) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	for _, route := range authorizedRoutes {
		if route.method == method && routePathMatches(route.path, path) {
			return route.scope, true
		}
	}
	return "", false
}

func routePathMatches(pattern, path string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) != len(pathParts) {
		return false
	}
	for index, part := range patternParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			if pathParts[index] == "" {
				return false
			}
			continue
		}
		if part != pathParts[index] {
			return false
		}
	}
	return true
}

type principalContextKey struct{}

// WithPrincipal attaches a detached federation principal to a request context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext returns the authenticated federation caller, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
