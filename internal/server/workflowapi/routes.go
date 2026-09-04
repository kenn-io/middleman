package workflowapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server/httpapi"
)

func (h *Handler) Register(api huma.API) {
	base := "/actions/{provider}/{owner}/{name}"
	hostBase := "/host/{platform_host}" + base
	register(api, "list-workflows", http.MethodGet, base+"/workflows", http.StatusOK, "List manual workflows", h.listCatalog)
	register(api, "list-workflows-on-host", http.MethodGet, hostBase+"/workflows", http.StatusOK, "List manual workflows", h.listCatalogOnHost)
	register(api, "list-workflow-runs", http.MethodGet, base+"/runs", http.StatusOK, "List workflow runs", h.listRuns)
	register(api, "list-workflow-runs-on-host", http.MethodGet, hostBase+"/runs", http.StatusOK, "List workflow runs", h.listRunsOnHost)
	register(api, "list-workflow-run-jobs", http.MethodGet, base+"/runs/{run_id}/jobs", http.StatusOK, "List workflow run jobs", h.listJobs)
	register(api, "list-workflow-run-jobs-on-host", http.MethodGet, hostBase+"/runs/{run_id}/jobs", http.StatusOK, "List workflow run jobs", h.listJobsOnHost)
	register(api, "dispatch-workflow", http.MethodPost, base+"/workflows/{workflow_id}/dispatch", http.StatusAccepted, "Dispatch workflow", h.dispatch)
	register(api, "dispatch-workflow-on-host", http.MethodPost, hostBase+"/workflows/{workflow_id}/dispatch", http.StatusAccepted, "Dispatch workflow", h.dispatchOnHost)
}

func register[I, O any](api huma.API, operationID, method, path string, status int, summary string, handler func(context.Context, *I) (*O, error)) {
	huma.Register(api, huma.Operation{
		OperationID: operationID, Method: method, Path: path, DefaultStatus: status,
		Summary: summary, Tags: []string{"Workflows"},
	}, handler)
}

type resolvedRepository struct {
	repo  *db.Repo
	fence db.RepositoryRouteFence
}

func (h *Handler) resolve(ctx context.Context, provider, host, owner, name, capability string) (resolvedRepository, error) {
	if h == nil || h.resolver == nil {
		return resolvedRepository{}, httpapi.Internal("repository resolver unavailable")
	}
	repo, err := h.resolver.RequireRouteCapability(ctx, provider, host, owner, name, capability)
	if err != nil {
		return resolvedRepository{}, err
	}
	fence, found, err := h.resolver.CaptureRepositoryRouteFence(ctx, *repo)
	if err != nil {
		return resolvedRepository{}, httpapi.Internal("capture repository identity failed")
	}
	if !found {
		return resolvedRepository{}, repositoryIdentityChangedProblem()
	}
	return resolvedRepository{repo: repo, fence: fence}, nil
}

func (h *Handler) confirm(ctx context.Context, resolved resolvedRepository) error {
	matches, err := h.resolver.RepositoryRouteFenceMatches(ctx, *resolved.repo, resolved.fence)
	if err != nil {
		return httpapi.Internal("confirm repository identity failed")
	}
	if !matches {
		return repositoryIdentityChangedProblem()
	}
	return nil
}

func repositoryIdentityChangedProblem() error {
	return httpapi.NotFound(httpapi.CodeRepoNotFound, "repository identity no longer matches this route", nil)
}

func (h *Handler) registry() *platform.Registry {
	if h == nil || h.syncer == nil {
		return nil
	}
	return h.syncer.Registry()
}

func (h *Handler) listCatalog(ctx context.Context, input *repositoryInput) (*catalogOutput, error) {
	resolved, err := h.resolve(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityReadWorkflows)
	if err != nil {
		return nil, err
	}
	registry := h.registry()
	if registry == nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflows)
	}
	reader, err := registry.WorkflowCatalogReader(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflows)
	}
	ref := httpapi.PlatformRepoRef(*resolved.repo)
	workflows, err := reader.ListManualWorkflows(ctx, ref)
	if err != nil {
		return nil, httpapi.ProviderCallProblem(err, string(ref.Platform), ref.Host)
	}
	var environments []platform.WorkflowEnvironment
	if workflowDefinitionsNeedEnvironments(workflows) {
		environments, err = reader.ListWorkflowEnvironments(ctx, ref)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, string(ref.Platform), ref.Host)
		}
	}
	if err := h.confirm(ctx, resolved); err != nil {
		return nil, err
	}
	repo := h.resolver.Ref(*resolved.repo)
	operations := h.operations(*resolved.repo)
	repo.Operations = &operations
	return &catalogOutput{Body: WorkflowCatalogResponse{
		Repo: repo, Workflows: workflowDefinitions(workflows), Environments: workflowEnvironments(environments),
	}}, nil
}

func (h *Handler) listCatalogOnHost(ctx context.Context, input *hostRepositoryInput) (*catalogOutput, error) {
	return h.listCatalog(ctx, &repositoryInput{Provider: input.Provider, PlatformHost: input.PlatformHost, Owner: input.Owner, Name: input.Name})
}

func (h *Handler) listRuns(ctx context.Context, input *workflowRunsInput) (*runsOutput, error) {
	if strings.TrimSpace(input.WorkflowID) == "" {
		return nil, httpapi.Validation("query.workflow_id", "workflow_id must not be blank")
	}
	resolved, err := h.resolve(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityReadWorkflowRuns)
	if err != nil {
		return nil, err
	}
	registry := h.registry()
	if registry == nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflowRuns)
	}
	reader, err := registry.WorkflowRunReader(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflowRuns)
	}
	query := platform.WorkflowRunQuery{
		WorkflowID: strings.TrimSpace(input.WorkflowID), Event: strings.TrimSpace(input.Event), Branch: strings.TrimSpace(input.Branch),
		Cursor: input.Cursor, PerPage: input.PerPage,
	}
	page, err := reader.ListWorkflowRuns(ctx, httpapi.PlatformRepoRef(*resolved.repo), query)
	if err != nil {
		return nil, httpapi.ProviderCallProblem(err, string(httpapi.ProviderKind(*resolved.repo)), httpapi.ProviderHost(*resolved.repo))
	}
	if err := h.confirm(ctx, resolved); err != nil {
		return nil, err
	}
	return &runsOutput{Body: WorkflowRunsResponse{
		Repo: h.resolver.Ref(*resolved.repo), Items: workflowRuns(page.Items), NextCursor: page.NextCursor, Exhausted: page.Exhausted,
	}}, nil
}

func (h *Handler) listRunsOnHost(ctx context.Context, input *hostWorkflowRunsInput) (*runsOutput, error) {
	return h.listRuns(ctx, &workflowRunsInput{
		Provider: input.Provider, PlatformHost: input.PlatformHost, Owner: input.Owner, Name: input.Name,
		WorkflowID: input.WorkflowID, Event: input.Event, Branch: input.Branch, Cursor: input.Cursor, PerPage: input.PerPage,
	})
}

func (h *Handler) listJobs(ctx context.Context, input *workflowJobsInput) (*jobsOutput, error) {
	runID := strings.TrimSpace(input.RunID)
	if runID == "" {
		return nil, httpapi.Validation("path.run_id", "run_id must not be blank")
	}
	resolved, err := h.resolve(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityReadWorkflowRuns)
	if err != nil {
		return nil, err
	}
	registry := h.registry()
	if registry == nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflowRuns)
	}
	reader, err := registry.WorkflowRunReader(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflowRuns)
	}
	jobs, err := reader.ListWorkflowRunJobs(ctx, httpapi.PlatformRepoRef(*resolved.repo), runID)
	if err != nil {
		return nil, httpapi.ProviderCallProblem(err, string(httpapi.ProviderKind(*resolved.repo)), httpapi.ProviderHost(*resolved.repo))
	}
	if err := h.confirm(ctx, resolved); err != nil {
		return nil, err
	}
	return &jobsOutput{Body: WorkflowJobsResponse{Repo: h.resolver.Ref(*resolved.repo), Items: workflowJobs(jobs)}}, nil
}

func (h *Handler) listJobsOnHost(ctx context.Context, input *hostWorkflowJobsInput) (*jobsOutput, error) {
	return h.listJobs(ctx, &workflowJobsInput{Provider: input.Provider, PlatformHost: input.PlatformHost, Owner: input.Owner, Name: input.Name, RunID: input.RunID})
}

func (h *Handler) dispatch(ctx context.Context, input *workflowDispatchInput) (*dispatchOutput, error) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, httpapi.Validation("path.workflow_id", "workflow_id must not be blank")
	}
	if strings.TrimSpace(input.Body.Ref) == "" {
		return nil, httpapi.Validation("body.ref", "ref must not be blank")
	}
	if len(input.Body.Inputs) > maxWorkflowInputs {
		return nil, httpapi.Validation("body.inputs", fmt.Sprintf("inputs must contain at most %d values", maxWorkflowInputs))
	}
	encodedInputs, err := json.Marshal(input.Body.Inputs)
	if err != nil {
		return nil, httpapi.Validation("body.inputs", "inputs must be JSON encodable")
	}
	if utf8.RuneCount(encodedInputs) > maxWorkflowInputPayload {
		return nil, httpapi.Validation("body.inputs", fmt.Sprintf("encoded inputs must not exceed %d characters", maxWorkflowInputPayload))
	}
	resolved, err := h.resolve(ctx, input.Provider, input.PlatformHost, input.Owner, input.Name, capabilityReadWorkflows)
	if err != nil {
		return nil, err
	}
	if !httpapi.CapabilityEnabled(h.resolver.Ref(*resolved.repo).Capabilities, capabilityWorkflowDispatch) {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityWorkflowDispatch)
	}
	registry := h.registry()
	if registry == nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityWorkflowDispatch)
	}
	catalogReader, err := registry.WorkflowCatalogReader(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityReadWorkflows)
	}
	dispatcher, err := registry.WorkflowDispatcher(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo))
	if err != nil {
		return nil, httpapi.UnsupportedCapability(*resolved.repo, capabilityWorkflowDispatch)
	}
	ref := httpapi.PlatformRepoRef(*resolved.repo)
	workflows, err := catalogReader.ListManualWorkflows(ctx, ref)
	if err != nil {
		return nil, httpapi.ProviderCallProblem(err, string(ref.Platform), ref.Host)
	}
	definition, found := findWorkflow(workflows, workflowID)
	if !found {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "workflow not found", map[string]any{"workflowId": workflowID})
	}
	if input.Body.ExpectedDefinitionSHA != definition.DefinitionSHA {
		return nil, httpapi.Conflict(httpapi.CodeConflict, "workflow definition changed", map[string]any{
			"reason": "workflow_definition_changed", "expectedDefinitionSha": input.Body.ExpectedDefinitionSHA, "definitionSha": definition.DefinitionSHA,
		})
	}
	if !definition.Available {
		return nil, httpapi.Conflict(httpapi.CodeConflict, "workflow is unavailable", map[string]any{"reason": definition.UnavailableReason})
	}
	var environments []platform.WorkflowEnvironment
	if workflowInputsNeedEnvironments(definition.Inputs) {
		environments, err = catalogReader.ListWorkflowEnvironments(ctx, ref)
		if err != nil {
			return nil, httpapi.ProviderCallProblem(err, string(ref.Platform), ref.Host)
		}
	}
	if err := validateWorkflowInputs(definition.Inputs, environments, input.Body.Inputs); err != nil {
		return nil, err
	}
	request := platform.WorkflowDispatchRequest{
		WorkflowID: workflowID, Ref: strings.TrimSpace(input.Body.Ref), Inputs: input.Body.Inputs,
		ExpectedDefinitionSHA: input.Body.ExpectedDefinitionSHA,
	}
	dispatchID := newDispatchID()
	startedAt := time.Now()
	var result platform.WorkflowDispatchResult
	matched, err := h.resolver.GuardRepositoryRouteFence(ctx, *resolved.repo, resolved.fence, func() error {
		availability := h.operations(*resolved.repo).DispatchWorkflow
		if !availability.Available {
			return dispatchUnavailableProblem(*resolved.repo, availability)
		}
		var dispatchErr error
		result, dispatchErr = dispatcher.DispatchWorkflow(ctx, ref, request)
		if dispatchErr != nil {
			problem := httpapi.ProviderMutationProblem(dispatchErr, string(ref.Platform), ref.Host)
			if result.Actor != "" {
				if mutationProblem, ok := problem.(*httpapi.ProblemError); ok && mutationProblem.Code == httpapi.CodeMutationOutcomeUnknown {
					if mutationProblem.Details == nil {
						mutationProblem.Details = map[string]any{}
					}
					mutationProblem.Details["actor"] = result.Actor
				}
			}
			return problem
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, repositoryIdentityChangedProblem()
	}
	response := WorkflowDispatchResponse{
		Accepted: result.Accepted, DispatchID: dispatchID, Actor: result.Actor,
	}
	if result.Run != nil {
		run := workflowRun(*result.Run)
		response.Run = &run
	}
	follow := dispatchFollow{
		repo: *resolved.repo, ref: ref, request: request, result: result,
		dispatchID: dispatchID, startedAt: startedAt,
	}
	if runReader, readerErr := registry.WorkflowRunReader(httpapi.ProviderKind(*resolved.repo), httpapi.ProviderHost(*resolved.repo)); readerErr == nil {
		follow.reader = runReader
	}
	h.background(func(ctx context.Context) { h.followDispatch(ctx, follow) })
	return &dispatchOutput{Status: http.StatusAccepted, Body: response}, nil
}

func (h *Handler) dispatchOnHost(ctx context.Context, input *hostWorkflowDispatchInput) (*dispatchOutput, error) {
	return h.dispatch(ctx, &workflowDispatchInput{
		Provider: input.Provider, PlatformHost: input.PlatformHost, Owner: input.Owner, Name: input.Name,
		WorkflowID: input.WorkflowID, Body: input.Body,
	})
}

func dispatchUnavailableProblem(repo db.Repo, availability httpapi.OperationAvailability) error {
	switch availability.Code {
	case "unsupported_capability":
		capability := availability.RequiredCapability
		if capability == "" {
			capability = capabilityWorkflowDispatch
		}
		return httpapi.UnsupportedCapability(repo, capability)
	case "rate_limited":
		details := map[string]any{"reason": "rate_limited", "provider": string(httpapi.ProviderKind(repo)), "platformHost": httpapi.ProviderHost(repo)}
		if availability.RetryAt != "" {
			details["retryAfter"] = availability.RetryAt
		}
		return httpapi.NewProblem(http.StatusTooManyRequests, httpapi.CodeRateLimited, availability.UnavailableReason, details)
	case "missing_write_credential", "write_credential_error":
		return httpapi.NewProblem(http.StatusForbidden, httpapi.CodeForbidden, availability.UnavailableReason, map[string]any{
			"reason": availability.Code, "provider": string(httpapi.ProviderKind(repo)), "platformHost": httpapi.ProviderHost(repo),
		})
	default:
		reason := availability.Code
		if reason == "" {
			reason = "operation_unavailable"
		}
		return httpapi.Conflict(httpapi.CodeConflict, availability.UnavailableReason, map[string]any{"reason": reason})
	}
}

func validateWorkflowInputs(definitions []platform.WorkflowInput, environments []platform.WorkflowEnvironment, inputs map[string]any) error {
	byName := make(map[string]platform.WorkflowInput, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name] = definition
	}
	for name := range inputs {
		if _, ok := byName[name]; !ok {
			return httpapi.Validation("body.inputs."+name, "unknown workflow input")
		}
	}
	environmentNames := make([]string, 0, len(environments))
	for _, environment := range environments {
		environmentNames = append(environmentNames, environment.Name)
	}
	for _, definition := range definitions {
		value, present := inputs[definition.Name]
		if !present {
			if definition.Required && !definition.HasDefault {
				return httpapi.Validation("body.inputs."+definition.Name, "required workflow input is missing")
			}
			continue
		}
		field := "body.inputs." + definition.Name
		switch definition.Type {
		case platform.WorkflowInputString:
			if _, ok := value.(string); !ok {
				return httpapi.Validation(field, "workflow input must be a string")
			}
		case platform.WorkflowInputBoolean:
			if _, ok := value.(bool); !ok {
				return httpapi.Validation(field, "workflow input must be a boolean")
			}
		case platform.WorkflowInputNumber:
			if !isJSONNumber(value) {
				return httpapi.Validation(field, "workflow input must be a number")
			}
		case platform.WorkflowInputChoice:
			choice, ok := value.(string)
			if !ok || !slices.Contains(definition.Options, choice) {
				return httpapi.Validation(field, "workflow input must be one of the declared choices", definition.Options...)
			}
		case platform.WorkflowInputEnvironment:
			environment, ok := value.(string)
			if !ok || !slices.Contains(environmentNames, environment) {
				return httpapi.Validation(field, "workflow input must name a live environment", environmentNames...)
			}
		default:
			return httpapi.Validation(field, "workflow input has an unsupported type")
		}
	}
	return nil
}

func isJSONNumber(value any) bool {
	switch value.(type) {
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return true
	default:
		return false
	}
}

func workflowInputsNeedEnvironments(inputs []platform.WorkflowInput) bool {
	for _, input := range inputs {
		if input.Type == platform.WorkflowInputEnvironment {
			return true
		}
	}
	return false
}

func workflowDefinitionsNeedEnvironments(workflows []platform.WorkflowDefinition) bool {
	for _, workflow := range workflows {
		if !workflow.Available {
			continue
		}
		if workflowInputsNeedEnvironments(workflow.Inputs) {
			return true
		}
	}
	return false
}

func findWorkflow(workflows []platform.WorkflowDefinition, id string) (platform.WorkflowDefinition, bool) {
	for _, workflow := range workflows {
		if workflow.ID == id {
			return workflow, true
		}
	}
	return platform.WorkflowDefinition{}, false
}

func workflowDefinitions(values []platform.WorkflowDefinition) []WorkflowDefinitionResponse {
	out := make([]WorkflowDefinitionResponse, 0, len(values))
	for _, value := range values {
		inputs := make([]WorkflowInputResponse, 0, len(value.Inputs))
		for _, input := range value.Inputs {
			inputs = append(inputs, WorkflowInputResponse{
				Name: input.Name, Description: input.Description, Required: input.Required, Type: input.Type,
				Default: input.Default, HasDefault: input.HasDefault, Options: slices.Clone(input.Options),
			})
		}
		out = append(out, WorkflowDefinitionResponse{
			ID: value.ID, Name: value.Name, Path: value.Path, State: value.State, WebURL: value.WebURL,
			DefinitionSHA: value.DefinitionSHA, Inputs: inputs, Available: value.Available, UnavailableReason: value.UnavailableReason,
		})
	}
	return out
}

func workflowEnvironments(values []platform.WorkflowEnvironment) []WorkflowEnvironmentResponse {
	out := make([]WorkflowEnvironmentResponse, 0, len(values))
	for _, value := range values {
		out = append(out, WorkflowEnvironmentResponse{Name: value.Name})
	}
	return out
}

func workflowRuns(values []platform.WorkflowRun) []WorkflowRunResponse {
	out := make([]WorkflowRunResponse, 0, len(values))
	for _, value := range values {
		out = append(out, workflowRun(value))
	}
	return out
}

func workflowRun(value platform.WorkflowRun) WorkflowRunResponse {
	return WorkflowRunResponse{
		ID: value.ID, WorkflowID: value.WorkflowID, RunNumber: value.RunNumber, Name: value.Name, Event: value.Event,
		Ref: value.Ref, HeadSHA: value.HeadSHA, Actor: value.Actor, Status: value.Status, Conclusion: value.Conclusion,
		CreatedAt: formatTime(value.CreatedAt), UpdatedAt: formatTime(value.UpdatedAt), WebURL: value.WebURL,
	}
}

func workflowJobs(values []platform.WorkflowRunJob) []WorkflowRunJobResponse {
	out := make([]WorkflowRunJobResponse, 0, len(values))
	for _, value := range values {
		steps := make([]WorkflowRunStepResponse, 0, len(value.Steps))
		for _, step := range value.Steps {
			steps = append(steps, WorkflowRunStepResponse{
				Number: step.Number, Name: step.Name, Status: step.Status, Conclusion: step.Conclusion,
				StartedAt: formatTime(step.StartedAt), CompletedAt: formatTime(step.CompletedAt),
			})
		}
		out = append(out, WorkflowRunJobResponse{
			ID: value.ID, Name: value.Name, Status: value.Status, Conclusion: value.Conclusion,
			StartedAt: formatTime(value.StartedAt), CompletedAt: formatTime(value.CompletedAt), WebURL: value.WebURL, Steps: steps,
		})
	}
	return out
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
