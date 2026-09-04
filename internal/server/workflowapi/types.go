package workflowapi

import (
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server/httpapi"
)

type repositoryInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
}

type hostRepositoryInput struct {
	Provider     string `path:"provider"`
	PlatformHost string `path:"platform_host"`
	Owner        string `path:"owner"`
	Name         string `path:"name"`
}

type workflowRunsInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	WorkflowID   string `query:"workflow_id" required:"true"`
	Event        string `query:"event"`
	Branch       string `query:"branch"`
	Cursor       string `query:"cursor"`
	PerPage      int    `query:"per_page" default:"20" minimum:"1" maximum:"100"`
}

type hostWorkflowRunsInput struct {
	Provider     string `path:"provider"`
	PlatformHost string `path:"platform_host"`
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	WorkflowID   string `query:"workflow_id" required:"true"`
	Event        string `query:"event"`
	Branch       string `query:"branch"`
	Cursor       string `query:"cursor"`
	PerPage      int    `query:"per_page" default:"20" minimum:"1" maximum:"100"`
}

type workflowJobsInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	RunID        string `path:"run_id"`
}

type hostWorkflowJobsInput struct {
	Provider     string `path:"provider"`
	PlatformHost string `path:"platform_host"`
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	RunID        string `path:"run_id"`
}

type workflowDispatchInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	WorkflowID   string `path:"workflow_id"`
	Body         workflowDispatchBody
}

type hostWorkflowDispatchInput struct {
	Provider     string `path:"provider"`
	PlatformHost string `path:"platform_host"`
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	WorkflowID   string `path:"workflow_id"`
	Body         workflowDispatchBody
}

type workflowDispatchBody struct {
	Ref                   string         `json:"ref"`
	Inputs                map[string]any `json:"inputs"`
	ExpectedDefinitionSHA string         `json:"expected_definition_sha"`
}

type WorkflowInputResponse struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	Required    bool                       `json:"required"`
	Type        platform.WorkflowInputType `json:"type" enum:"string,number,boolean,choice,environment"`
	Default     any                        `json:"default,omitempty"`
	HasDefault  bool                       `json:"has_default"`
	Options     []string                   `json:"options,omitempty"`
}

type WorkflowDefinitionResponse struct {
	ID                string                  `json:"id"`
	Name              string                  `json:"name"`
	Path              string                  `json:"path"`
	State             string                  `json:"state"`
	WebURL            string                  `json:"web_url" format:"uri"`
	DefinitionSHA     string                  `json:"definition_sha"`
	Inputs            []WorkflowInputResponse `json:"inputs"`
	Available         bool                    `json:"available"`
	UnavailableReason string                  `json:"unavailable_reason,omitempty"`
}

type WorkflowEnvironmentResponse struct {
	Name string `json:"name"`
}

type WorkflowCatalogResponse struct {
	Repo         httpapi.RepoRefResponse       `json:"repo"`
	Workflows    []WorkflowDefinitionResponse  `json:"workflows"`
	Environments []WorkflowEnvironmentResponse `json:"environments"`
}

type WorkflowRunStepResponse struct {
	Number      int    `json:"number"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at,omitempty" format:"date-time"`
	CompletedAt string `json:"completed_at,omitempty" format:"date-time"`
}

type WorkflowRunJobResponse struct {
	ID          string                    `json:"id"`
	Name        string                    `json:"name"`
	Status      string                    `json:"status"`
	Conclusion  string                    `json:"conclusion"`
	StartedAt   string                    `json:"started_at,omitempty" format:"date-time"`
	CompletedAt string                    `json:"completed_at,omitempty" format:"date-time"`
	WebURL      string                    `json:"web_url,omitempty" format:"uri"`
	Steps       []WorkflowRunStepResponse `json:"steps"`
}

type WorkflowRunResponse struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	RunNumber  int64  `json:"run_number"`
	Name       string `json:"name"`
	Event      string `json:"event"`
	Ref        string `json:"ref"`
	HeadSHA    string `json:"head_sha"`
	Actor      string `json:"actor"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CreatedAt  string `json:"created_at,omitempty" format:"date-time"`
	UpdatedAt  string `json:"updated_at,omitempty" format:"date-time"`
	WebURL     string `json:"web_url,omitempty" format:"uri"`
}

type WorkflowRunsResponse struct {
	Repo       httpapi.RepoRefResponse `json:"repo"`
	Items      []WorkflowRunResponse   `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Exhausted  bool                    `json:"exhausted"`
}

type WorkflowJobsResponse struct {
	Repo  httpapi.RepoRefResponse  `json:"repo"`
	Items []WorkflowRunJobResponse `json:"items"`
}

// WorkflowDispatchResponse acknowledges an accepted dispatch. Run is present only
// when the provider named the run immediately; otherwise the server locates it
// and reports through workflow_dispatch_progress events keyed by DispatchID.
type WorkflowDispatchResponse struct {
	Accepted   bool                 `json:"accepted"`
	DispatchID string               `json:"dispatch_id"`
	Actor      string               `json:"actor,omitempty"`
	Run        *WorkflowRunResponse `json:"run,omitempty"`
}

type catalogOutput = httpapi.BodyOutput[WorkflowCatalogResponse]
type runsOutput = httpapi.BodyOutput[WorkflowRunsResponse]
type jobsOutput = httpapi.BodyOutput[WorkflowJobsResponse]
type dispatchOutput = httpapi.AcceptedBodyOutput[WorkflowDispatchResponse]
