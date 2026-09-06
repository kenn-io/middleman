package fleetapi

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// registerFleetProjectRoutes registers host-targeted project
// register/delete on the hub, dispatched like every other fleet
// write: local handler or authenticated member proxy.
func (s *Handler) registerFleetProjectRoutes(api huma.API) {
	registerOp := &huma.Operation{
		OperationID:  "register-fleet-project",
		Method:       http.MethodPost,
		Path:         "/fleet/hosts/{host_key}/projects",
		Summary:      "Register project on fleet host",
		Tags:         []string{"Fleet"},
		Parameters:   fleetProxyParams([]string{"host_key"}),
		RequestBody:  fleetProxyRequestBody(),
		Responses:    fleetProxyResponses(),
		MaxBodyBytes: fleetProxyMaxBodyBytes,
	}
	api.OpenAPI().AddOperation(registerOp)
	api.Adapter().Handle(registerOp, func(ctx huma.Context) {
		r, w := humago.Unwrap(ctx)
		if !bufferFleetProxyRequestBody(w, r, fleetProxyMaxBodyBytes) {
			return
		}
		s.serveFleetProjectWrite(w, r, "/api/v1/projects")
	})

	deleteOp := &huma.Operation{
		OperationID:  "delete-fleet-project",
		Method:       http.MethodDelete,
		Path:         "/fleet/hosts/{host_key}/projects/{project_id}",
		Summary:      "Delete project on fleet host",
		Tags:         []string{"Fleet"},
		Parameters:   fleetProxyParams([]string{"host_key", "project_id"}),
		Responses:    fleetProxyResponses(),
		MaxBodyBytes: fleetProxyMaxBodyBytes,
	}
	api.OpenAPI().AddOperation(deleteOp)
	api.Adapter().Handle(deleteOp, func(ctx huma.Context) {
		r, w := humago.Unwrap(ctx)
		if !bufferFleetProxyRequestBody(w, r, fleetProxyMaxBodyBytes) {
			return
		}
		projectID := r.PathValue("project_id")
		s.serveFleetProjectWrite(
			w, r, "/api/v1/projects/"+escapePath(projectID),
		)
	})
}

// serveFleetProjectWrite routes a host-targeted project write: the
// local host runs the existing local handler, an active member is forwarded
// over authenticated HTTP, and an unknown host is a 404.
func (s *Handler) serveFleetProjectWrite(
	w http.ResponseWriter,
	r *http.Request,
	localPath string,
) {
	hostKey := r.PathValue("host_key")
	target, ok := s.resolveFleetHostTarget(hostKey)
	if !ok {
		writeProblemResponse(w, fleetHostNotFoundProblem(hostKey))
		return
	}
	if target.self {
		s.serveLocalFleetRESTProxy(w, r, localPath)
		return
	}
	s.serveRemoteFleetRESTProxy(w, r, target, localPath)
}
