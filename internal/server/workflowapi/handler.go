// Package workflowapi owns provider-neutral workflow catalog, run, job, and dispatch HTTP behavior.
package workflowapi

import (
	"context"

	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server/httpapi"
)

const (
	capabilityReadWorkflows    = "read_workflows"
	capabilityReadWorkflowRuns = "read_workflow_runs"
	capabilityWorkflowDispatch = "workflow_dispatch"
)

const (
	maxWorkflowInputs       = 25
	maxWorkflowInputPayload = 65_535
)

// Runtime is what dispatch follow-through needs from the hosting server:
// publishing server-sent events and running work that outlives the request.
type Runtime interface {
	// Publish sends an event to every connected client.
	Publish(eventType string, data any)
	// Go runs fn on a server-owned goroutine whose context ends at shutdown and
	// reports whether it started.
	Go(fn func(context.Context)) bool
}

type Deps struct {
	Resolver       *httpapi.RepositoryResolver
	Syncer         *ghclient.Syncer
	RepoOperations func(db.Repo) httpapi.RepoOperations
	// Runtime may be nil, in which case dispatch follow-through runs inline
	// and publishes nothing.
	Runtime Runtime
}

type Handler struct {
	resolver       *httpapi.RepositoryResolver
	syncer         *ghclient.Syncer
	repoOperations func(db.Repo) httpapi.RepoOperations
	runtime        Runtime
	follow         dispatchFollowConfig
}

func New(deps Deps) *Handler {
	return &Handler{
		resolver:       deps.Resolver,
		syncer:         deps.Syncer,
		repoOperations: deps.RepoOperations,
		runtime:        deps.Runtime,
		follow:         defaultDispatchFollowConfig,
	}
}

func (h *Handler) operations(repo db.Repo) httpapi.RepoOperations {
	if h.repoOperations == nil {
		return httpapi.RepoOperations{}
	}
	return h.repoOperations(repo)
}

func (h *Handler) publish(eventType string, data any) {
	if h.runtime == nil {
		return
	}
	h.runtime.Publish(eventType, data)
}

func (h *Handler) background(fn func(context.Context)) {
	if h.runtime == nil {
		fn(context.Background())
		return
	}
	h.runtime.Go(fn)
}
