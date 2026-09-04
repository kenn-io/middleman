package workflowapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

type workflowTestProvider struct {
	caps              platform.Capabilities
	catalog           []platform.WorkflowDefinition
	authenticatedUser string
	catalogCalls      int
	environmentCalls  int
	environments      []platform.WorkflowEnvironment
	runs              platform.Page[platform.WorkflowRun]
	jobs              []platform.WorkflowRunJob
	dispatch          platform.WorkflowDispatchResult
	catalogErr        error
	runsErr           error
	jobsErr           error
	dispatchErr       error
	dispatches        []platform.WorkflowDispatchRequest
	onCatalog         func()
	runQueries        []platform.WorkflowRunQuery
	followRuns        func(call int) platform.Page[platform.WorkflowRun]
}

func (p *workflowTestProvider) Platform() platform.Kind             { return platform.KindGitHub }
func (p *workflowTestProvider) Host() string                        { return platform.DefaultGitHubHost }
func (p *workflowTestProvider) Capabilities() platform.Capabilities { return p.caps }
func (p *workflowTestProvider) AuthenticatedUser(context.Context, platform.RepoRef) (string, error) {
	return p.authenticatedUser, nil
}
func (p *workflowTestProvider) ListManualWorkflows(context.Context, platform.RepoRef) ([]platform.WorkflowDefinition, error) {
	p.catalogCalls++
	if p.onCatalog != nil {
		p.onCatalog()
	}
	return p.catalog, p.catalogErr
}
func (p *workflowTestProvider) ListWorkflowEnvironments(context.Context, platform.RepoRef) ([]platform.WorkflowEnvironment, error) {
	p.environmentCalls++
	return p.environments, nil
}
func (p *workflowTestProvider) ListWorkflowRuns(_ context.Context, _ platform.RepoRef, query platform.WorkflowRunQuery) (platform.Page[platform.WorkflowRun], error) {
	p.runQueries = append(p.runQueries, query)
	if p.followRuns != nil && query.Cursor == "" {
		return p.followRuns(len(p.runQueries)), nil
	}
	if query.PerPage != 20 || query.Cursor != "cursor-1" || query.WorkflowID != "release.yml" || query.Event != "workflow_dispatch" || query.Branch != "main" {
		return platform.Page[platform.WorkflowRun]{}, &platform.Error{Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub, PlatformHost: platform.DefaultGitHubHost, Field: "query", Err: errors.New("query mismatch")}
	}
	return p.runs, p.runsErr
}
func (p *workflowTestProvider) ListWorkflowRunJobs(context.Context, platform.RepoRef, string) ([]platform.WorkflowRunJob, error) {
	return p.jobs, p.jobsErr
}
func (p *workflowTestProvider) DispatchWorkflow(_ context.Context, _ platform.RepoRef, request platform.WorkflowDispatchRequest) (platform.WorkflowDispatchResult, error) {
	p.dispatches = append(p.dispatches, request)
	return p.dispatch, p.dispatchErr
}

// workflowTestRuntime runs background work inline and records published events.
type workflowTestRuntime struct {
	events []WorkflowDispatchProgressPayload
}

func (r *workflowTestRuntime) Publish(eventType string, data any) {
	if eventType != EventTypeWorkflowDispatchProgress {
		return
	}
	r.events = append(r.events, data.(WorkflowDispatchProgressPayload))
}

func (r *workflowTestRuntime) Go(fn func(context.Context)) bool {
	fn(context.Background())
	return true
}

func workflowFixture(t *testing.T, provider *workflowTestProvider, operation httpapi.OperationAvailability) (*http.ServeMux, *db.DB) {
	t.Helper()
	mux, database, _ := workflowFixtureWithRuntime(t, provider, operation)
	return mux, database
}

func workflowFixtureWithRuntime(t *testing.T, provider *workflowTestProvider, operation httpapi.OperationAvailability) (*http.ServeMux, *db.DB, *Handler) {
	t.Helper()
	database := dbtest.Open(t)
	identity := db.GitHubRepoIdentity(platform.DefaultGitHubHost, "acme", "widget")
	identity.PlatformRepoID = "R_widget"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(t, err)
	require.NoError(t, database.UpdateRepoProviderMetadata(t.Context(), repoID, db.RepoProviderMetadata{
		PlatformRepoID: "R_widget",
		DefaultBranch:  "trunk",
	}))
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Minute, nil, nil)
	t.Cleanup(syncer.Stop)
	resolver := httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{
		DB:                   database,
		ProviderCapabilities: func(platform.Kind, string) (platform.Capabilities, error) { return provider.caps, nil },
	})
	handler := New(Deps{
		Resolver:       resolver,
		Syncer:         syncer,
		RepoOperations: func(db.Repo) httpapi.RepoOperations { return httpapi.RepoOperations{DispatchWorkflow: operation} },
		Runtime:        &workflowTestRuntime{},
	})
	handler.follow = dispatchFollowConfig{
		locateInitialDelay: 0, locateInterval: time.Millisecond, locateTimeout: 50 * time.Millisecond,
		clockSkew: 5 * time.Second, watchInterval: time.Millisecond, watchTimeout: 50 * time.Millisecond,
	}
	mux := http.NewServeMux()
	config := huma.DefaultConfig("workflow test", "0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.NewWithPrefix(mux, "/api/v1", config)
	handler.Register(api)
	return mux, database, handler
}

func publishedDispatchEvents(handler *Handler) []WorkflowDispatchProgressPayload {
	return handler.runtime.(*workflowTestRuntime).events
}

func workflowRequest(t *testing.T, mux http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	req := httptest.NewRequest(method, "/api/v1"+path, &payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded), recorder.Body.String())
	return recorder.Code, decoded
}

func workflowDefinitionFixture() platform.WorkflowDefinition {
	return platform.WorkflowDefinition{
		ID: "release.yml", Name: "Release", Path: ".github/workflows/release.yml", State: "active",
		WebURL: "https://github.com/acme/widget/actions/workflows/release.yml", DefinitionSHA: "definition-v1",
		Available: true,
		Inputs: []platform.WorkflowInput{
			{Name: "version", Required: true, Type: platform.WorkflowInputString},
			{Name: "dry_run", Type: platform.WorkflowInputBoolean, Default: false, HasDefault: true},
			{Name: "retries", Type: platform.WorkflowInputNumber, Default: 2, HasDefault: true},
			{Name: "channel", Type: platform.WorkflowInputChoice, Options: []string{"stable", "beta"}},
			{Name: "target", Type: platform.WorkflowInputEnvironment},
		},
	}
}

func workflowDefinitionWithoutEnvironmentFixture() platform.WorkflowDefinition {
	definition := workflowDefinitionFixture()
	inputs := make([]platform.WorkflowInput, 0, len(definition.Inputs))
	for _, input := range definition.Inputs {
		if input.Type != platform.WorkflowInputEnvironment {
			inputs = append(inputs, input)
		}
	}
	definition.Inputs = inputs
	return definition
}

func TestWorkflowCatalogRoutesUseStableRepositoryIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &workflowTestProvider{
		caps:         platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		catalog:      []platform.WorkflowDefinition{workflowDefinitionFixture()},
		environments: []platform.WorkflowEnvironment{{Name: "production"}, {Name: "staging"}},
	}
	mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
	for _, path := range []string{
		"/actions/github/acme/widget/workflows",
		"/host/github.com/actions/github/acme/widget/workflows",
	} {
		status, body := workflowRequest(t, mux, http.MethodGet, path, nil)
		require.Equal(http.StatusOK, status)
		repo := body["repo"].(map[string]any)
		assert.Equal("R_widget", repo["platform_repo_id"])
		assert.Equal("acme/widget", repo["repo_path"])
		assert.Equal("trunk", repo["default_branch"])
		assert.Equal(true, repo["operations"].(map[string]any)["dispatch_workflow"].(map[string]any)["available"])
		workflow := body["workflows"].([]any)[0].(map[string]any)
		assert.Equal("definition-v1", workflow["definition_sha"])
		assert.Equal(false, workflow["inputs"].([]any)[1].(map[string]any)["default"])
		assert.InDelta(float64(2), workflow["inputs"].([]any)[2].(map[string]any)["default"], 0)
		assert.Equal("production", body["environments"].([]any)[0].(map[string]any)["name"])
	}
}

func TestWorkflowRoutesLoadEnvironmentsOnlyForDefinitionsThatNeedThem(t *testing.T) {
	tests := []struct {
		name             string
		definition       platform.WorkflowDefinition
		method           string
		path             string
		body             map[string]any
		wantEnvironments int
	}{
		{
			name:       "catalog without environment input",
			definition: workflowDefinitionWithoutEnvironmentFixture(),
			method:     http.MethodGet,
			path:       "/actions/github/acme/widget/workflows",
		},
		{
			name:             "catalog with environment input",
			definition:       workflowDefinitionFixture(),
			method:           http.MethodGet,
			path:             "/actions/github/acme/widget/workflows",
			wantEnvironments: 1,
		},
		{
			name:       "dispatch without environment input",
			definition: workflowDefinitionWithoutEnvironmentFixture(),
			method:     http.MethodPost,
			path:       "/actions/github/acme/widget/workflows/release.yml/dispatch",
			body: map[string]any{
				"ref":                     "main",
				"expected_definition_sha": "definition-v1",
				"inputs":                  map[string]any{"version": "1"},
			},
		},
		{
			name:       "dispatch with environment input",
			definition: workflowDefinitionFixture(),
			method:     http.MethodPost,
			path:       "/actions/github/acme/widget/workflows/release.yml/dispatch",
			body: map[string]any{
				"ref":                     "main",
				"expected_definition_sha": "definition-v1",
				"inputs":                  map[string]any{"version": "1", "target": "production"},
			},
			wantEnvironments: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			provider := &workflowTestProvider{
				caps:         platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true},
				catalog:      []platform.WorkflowDefinition{test.definition},
				environments: []platform.WorkflowEnvironment{{Name: "production"}},
				dispatch:     platform.WorkflowDispatchResult{Accepted: true},
			}
			mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
			status, _ := workflowRequest(t, mux, test.method, test.path, test.body)
			if test.method == http.MethodPost {
				assert.Equal(http.StatusAccepted, status)
				assert.Len(provider.dispatches, 1)
			} else {
				assert.Equal(http.StatusOK, status)
			}
			assert.Equal(1, provider.catalogCalls)
			assert.Equal(test.wantEnvironments, provider.environmentCalls)
		})
	}
}

func TestWorkflowCatalogIgnoresUnavailableEnvironmentInputs(t *testing.T) {
	assert := assert.New(t)
	unavailable := workflowDefinitionFixture()
	unavailable.ID = "broken.yml"
	unavailable.Name = "Broken"
	unavailable.Available = false
	unavailable.UnavailableReason = "definition unavailable"
	provider := &workflowTestProvider{
		caps: platform.Capabilities{ReadWorkflows: true},
		catalog: []platform.WorkflowDefinition{
			workflowDefinitionWithoutEnvironmentFixture(),
			unavailable,
		},
	}
	mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{})
	status, body := workflowRequest(t, mux, http.MethodGet, "/actions/github/acme/widget/workflows", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Len(body["workflows"], 2)
	assert.Equal(1, provider.catalogCalls)
	assert.Zero(provider.environmentCalls)
}

func TestWorkflowRunRoutesRequireWorkflowIDInSchema(t *testing.T) {
	mux := http.NewServeMux()
	config := huma.DefaultConfig("workflow schema test", "0")
	config.OpenAPIPath, config.DocsPath, config.SchemasPath = "", "", ""
	api := humago.NewWithPrefix(mux, "/api/v1", config)
	New(Deps{}).Register(api)

	for _, path := range []string{
		"/actions/{provider}/{owner}/{name}/runs",
		"/host/{platform_host}/actions/{provider}/{owner}/{name}/runs",
	} {
		operation := api.OpenAPI().Paths[path].Get
		require.NotNil(t, operation)
		var workflowID *huma.Param
		for _, parameter := range operation.Parameters {
			if parameter.Name == "workflow_id" && parameter.In == "query" {
				workflowID = parameter
				break
			}
		}
		require.NotNil(t, workflowID)
		assert.True(t, workflowID.Required)
	}
}

func TestWorkflowRunsAndJobsPreserveProviderContracts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	started, err := time.Parse(time.RFC3339, "2026-08-27T14:30:00+02:00")
	require.NoError(err)
	provider := &workflowTestProvider{
		caps: platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		runs: platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{{ID: "41", WorkflowID: "release.yml", RunNumber: 7, CreatedAt: started, UpdatedAt: started.Add(time.Minute)}}, NextCursor: "cursor-2", Exhausted: false},
		jobs: []platform.WorkflowRunJob{
			{ID: "job-2", Name: "deploy", StartedAt: started.Add(time.Minute)},
			{ID: "job-1", Name: "build", StartedAt: started},
		},
	}
	mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
	status, body := workflowRequest(t, mux, http.MethodGet, "/actions/github/acme/widget/runs?workflow_id=release.yml&event=workflow_dispatch&branch=main&cursor=cursor-1&per_page=20", nil)
	require.Equal(http.StatusOK, status)
	assert.Equal("cursor-2", body["next_cursor"])
	assert.Equal(false, body["exhausted"])
	assert.Equal("2026-08-27T12:30:00Z", body["items"].([]any)[0].(map[string]any)["created_at"])
	status, body = workflowRequest(t, mux, http.MethodGet, "/host/github.com/actions/github/acme/widget/runs/41/jobs", nil)
	require.Equal(http.StatusOK, status)
	items := body["items"].([]any)
	assert.Equal("job-2", items[0].(map[string]any)["id"])
	assert.Equal("2026-08-27T12:31:00Z", items[0].(map[string]any)["started_at"])
	assert.Equal("job-1", items[1].(map[string]any)["id"])
}

func TestWorkflowRoutesRejectUnsupportedAndMalformedIdentifiers(t *testing.T) {
	assert := assert.New(t)
	provider := &workflowTestProvider{caps: platform.Capabilities{}}
	mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{})
	status, body := workflowRequest(t, mux, http.MethodGet, "/actions/github/acme/widget/workflows", nil)
	assert.Equal(http.StatusConflict, status)
	assert.Equal("unsupportedCapability", body["code"])
	status, body = workflowRequest(t, mux, http.MethodGet, "/actions/github/acme/widget/runs?workflow_id=release.yml", nil)
	assert.Equal(http.StatusConflict, status)
	assert.Equal("read_workflow_runs", body["details"].(map[string]any)["capability"])

	provider.caps = platform.Capabilities{ReadWorkflows: true}

	status, body = workflowRequest(
		t,
		mux,
		http.MethodPost,
		"/actions/github/acme/widget/workflows/release.yml/dispatch",
		map[string]any{
			"ref":                     "main",
			"expected_definition_sha": "definition-v1",
			"inputs":                  map[string]any{},
		},
	)
	assert.Equal(http.StatusConflict, status)
	assert.Equal("workflow_dispatch", body["details"].(map[string]any)["capability"])

	provider.caps = platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true}
	for _, path := range []string{
		"/actions/github/acme/widget/runs?workflow_id=%20",
		"/actions/github/acme/widget/runs/%20/jobs",
	} {
		status, body = workflowRequest(t, mux, http.MethodGet, path, nil)
		assert.Equal(http.StatusBadRequest, status)
		assert.Equal("validationError", body["code"])
		assert.NotEmpty(body["details"].(map[string]any)["field"])
	}
	for _, path := range []string{
		"/actions/github/acme/widget/runs",
		"/host/github.com/actions/github/acme/widget/runs",
	} {
		status, body = workflowRequest(t, mux, http.MethodGet, path, nil)
		assert.Equal(http.StatusUnprocessableEntity, status)
		assert.Equal("validationError", body["code"])
	}
	status, body = workflowRequest(
		t,
		mux,
		http.MethodPost,
		"/actions/github/acme/widget/workflows/%20/dispatch",
		map[string]any{
			"ref":                     "main",
			"expected_definition_sha": "definition-v1",
			"inputs":                  map[string]any{},
		},
	)
	assert.Equal(http.StatusBadRequest, status)
	assert.Equal("validationError", body["code"])
	assert.Equal("path.workflow_id", body["details"].(map[string]any)["field"])
}

func TestWorkflowCatalogFailsClosedWhenRouteIdentityChanges(t *testing.T) {
	provider := &workflowTestProvider{caps: platform.Capabilities{ReadWorkflows: true}, catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()}}
	mux, database := workflowFixture(t, provider, httpapi.OperationAvailability{})
	provider.onCatalog = func() {
		now := time.Now().UTC()
		_, _, err := database.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{Platform: "github", PlatformHost: "github.com", PlatformRepoID: "R_widget", Owner: "acme", Name: "renamed"}, now)
		require.NoError(t, err)
		_, _, err = database.ReconcileRepositoryObservation(t.Context(), db.RepoIdentity{Platform: "github", PlatformHost: "github.com", PlatformRepoID: "R_replacement", Owner: "acme", Name: "widget"}, now.Add(time.Second))
		require.NoError(t, err)
	}
	status, body := workflowRequest(t, mux, http.MethodGet, "/actions/github/acme/widget/workflows", nil)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "repoNotFound", body["code"])
}

func TestWorkflowDispatchValidatesLiveDefinitionBeforeMutation(t *testing.T) {
	tests := []struct {
		name       string
		body       map[string]any
		wantField  string
		wantStatus int
		wantReason string
	}{
		{name: "blank ref", body: map[string]any{"ref": " ", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}}, wantField: "body.ref", wantStatus: 400},
		{name: "unknown input", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1", "extra": "x"}}, wantField: "body.inputs.extra", wantStatus: 400},
		{name: "missing required", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{}}, wantField: "body.inputs.version", wantStatus: 400},
		{name: "wrong boolean", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1", "dry_run": "false"}}, wantField: "body.inputs.dry_run", wantStatus: 400},
		{name: "wrong number", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1", "retries": "2"}}, wantField: "body.inputs.retries", wantStatus: 400},
		{name: "invalid choice", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1", "channel": "nightly"}}, wantField: "body.inputs.channel", wantStatus: 400},
		{name: "invalid environment", body: map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1", "target": "qa"}}, wantField: "body.inputs.target", wantStatus: 400},
		{name: "stale definition", body: map[string]any{"ref": "main", "expected_definition_sha": "old", "inputs": map[string]any{"version": "1"}}, wantStatus: 409, wantReason: "workflow_definition_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			provider := &workflowTestProvider{caps: platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true}, catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()}, environments: []platform.WorkflowEnvironment{{Name: "production"}}}
			mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
			status, body := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", test.body)
			assert.Equal(test.wantStatus, status)
			assert.Empty(provider.dispatches)
			if test.wantField != "" {
				assert.Equal(test.wantField, body["details"].(map[string]any)["field"])
			}
			if test.wantReason != "" {
				assert.Equal(test.wantReason, body["details"].(map[string]any)["reason"])
			}
		})
	}
}

func TestWorkflowDispatchEnforcesInputLimitsAndOperationGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &workflowTestProvider{caps: platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true}, catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()}}
	mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
	many := map[string]any{}
	for i := range 26 {
		many[string(rune('a'+i))] = "x"
	}
	for _, inputs := range []map[string]any{many, {"version": strings.Repeat("x", 65536)}} {
		status, _ := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": inputs})
		assert.Equal(http.StatusBadRequest, status)
		assert.Empty(provider.dispatches)
	}
	status, _ := workflowRequest(
		t,
		mux,
		http.MethodPost,
		"/actions/github/acme/widget/workflows/release.yml/dispatch",
		map[string]any{
			"ref":                     "main",
			"expected_definition_sha": "definition-v1",
			"inputs":                  map[string]any{"version": strings.Repeat("é", 32761)},
		},
	)
	assert.Equal(http.StatusAccepted, status)
	require.Len(provider.dispatches, 1)
	provider.dispatches = nil
	mux, _ = workflowFixture(t, provider, httpapi.OperationAvailability{Available: false, Code: "rate_limited", UnavailableReason: "REST rate limit exhausted"})
	status, body := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
	assert.Equal(http.StatusTooManyRequests, status)
	assert.Equal("rateLimited", body["code"])
	assert.Empty(provider.dispatches)
}
func TestWorkflowDispatchMapsWriteCredentialAvailabilityToForbidden(t *testing.T) {
	for _, reason := range []string{"missing_write_credential", "write_credential_error"} {
		t.Run(reason, func(t *testing.T) {
			assert := assert.New(t)
			provider := &workflowTestProvider{
				caps:    platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true},
				catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()},
			}
			mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{
				Code: reason, UnavailableReason: "Personal write credential unavailable.",
			})
			status, body := workflowRequest(
				t,
				mux,
				http.MethodPost,
				"/actions/github/acme/widget/workflows/release.yml/dispatch",
				map[string]any{
					"ref":                     "main",
					"expected_definition_sha": "definition-v1",
					"inputs":                  map[string]any{"version": "1"},
				},
			)
			assert.Equal(http.StatusForbidden, status)
			assert.Equal("forbidden", body["code"])
			assert.Equal(reason, body["details"].(map[string]any)["reason"])
			assert.Equal("github", body["details"].(map[string]any)["provider"])
			assert.Equal("github.com", body["details"].(map[string]any)["platformHost"])
			assert.Empty(provider.dispatches)
		})
	}
}

func TestWorkflowDispatchMapsProviderRejectionAndMutationUncertainty(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantActor  bool
	}{
		{name: "typed rejection", err: &platform.Error{Code: platform.ErrCodeInvalidArgument, Provider: platform.KindGitHub, PlatformHost: platform.DefaultGitHubHost, Field: "ref", Err: errors.New("ref rejected")}, wantStatus: 400, wantCode: "badRequest"},
		{name: "transport uncertainty", err: errors.New("connection reset"), wantStatus: 502, wantCode: "mutationOutcomeUnknown", wantActor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			provider := &workflowTestProvider{
				caps:              platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true, ReadAuthenticatedUser: true},
				catalog:           []platform.WorkflowDefinition{workflowDefinitionFixture()},
				dispatch:          platform.WorkflowDispatchResult{Actor: "maintainer"},
				dispatchErr:       test.err,
				authenticatedUser: "maintainer",
			}
			mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
			status, body := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
			assert.Equal(test.wantStatus, status)
			assert.Equal(test.wantCode, body["code"])
			if test.wantActor {
				assert.Equal("maintainer", body["details"].(map[string]any)["actor"])
			}
			require.Len(provider.dispatches, 1)
		})
	}
}

func TestWorkflowDispatchResponseCarriesDispatchIDAndKnownRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, result := range []platform.WorkflowDispatchResult{
		{Accepted: true, Actor: "maintainer"},
		{Accepted: true, Actor: "maintainer", Run: &platform.WorkflowRun{ID: "run-9", WorkflowID: "release.yml", CreatedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)}},
	} {
		provider := &workflowTestProvider{caps: platform.Capabilities{ReadWorkflows: true, WorkflowDispatch: true}, catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()}, dispatch: result}
		mux, _ := workflowFixture(t, provider, httpapi.OperationAvailability{Available: true})
		status, body := workflowRequest(t, mux, http.MethodPost, "/host/github.com/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
		require.Equal(http.StatusAccepted, status)
		assert.NotEmpty(body["dispatch_id"])
		assert.Equal("maintainer", body["actor"])
		if result.Run != nil {
			assert.Equal("run-9", body["run"].(map[string]any)["id"])
		} else {
			assert.Nil(body["run"])
		}
	}
}

func TestWorkflowDispatchFollowThroughLocatesAndWatchesRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dispatchedAt := time.Now()
	staleRun := platform.WorkflowRun{ID: "old-run", WorkflowID: "release.yml", Actor: "maintainer", Status: "completed", Conclusion: "success", CreatedAt: dispatchedAt.Add(-time.Hour)}
	otherActorRun := platform.WorkflowRun{ID: "someone-else", WorkflowID: "release.yml", Actor: "colleague", Status: "queued", CreatedAt: dispatchedAt.Add(time.Second)}
	newRun := platform.WorkflowRun{ID: "new-run", WorkflowID: "release.yml", Actor: "maintainer", Status: "queued", CreatedAt: dispatchedAt.Add(time.Second)}
	provider := &workflowTestProvider{
		caps:     platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		catalog:  []platform.WorkflowDefinition{workflowDefinitionFixture()},
		dispatch: platform.WorkflowDispatchResult{Accepted: true, Actor: "maintainer"},
	}
	provider.followRuns = func(call int) platform.Page[platform.WorkflowRun] {
		switch call {
		case 1:
			return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{staleRun, otherActorRun}}
		case 2:
			return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{newRun, staleRun, otherActorRun}}
		case 3:
			progressed := newRun
			progressed.Status = "in_progress"
			progressed.UpdatedAt = dispatchedAt.Add(2 * time.Second)
			return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{progressed}}
		default:
			finished := newRun
			finished.Status, finished.Conclusion = "completed", "success"
			finished.UpdatedAt = dispatchedAt.Add(3 * time.Second)
			return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{finished}}
		}
	}
	mux, _, handler := workflowFixtureWithRuntime(t, provider, httpapi.OperationAvailability{Available: true})

	status, body := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
	require.Equal(http.StatusAccepted, status)
	dispatchID := body["dispatch_id"].(string)
	require.NotEmpty(dispatchID)

	events := publishedDispatchEvents(handler)
	require.Len(events, 3, "located, updated, then terminal updated")
	for _, event := range events {
		assert.Equal(dispatchID, event.DispatchID)
		assert.Equal("release.yml", event.WorkflowID)
		assert.Equal("acme", event.Owner)
		assert.Equal("widget", event.Name)
		require.NotNil(event.Run)
		assert.Equal("new-run", event.Run.ID)
	}
	assert.Equal("located", events[0].Status)
	assert.Equal("queued", events[0].Run.Status)
	assert.Equal("updated", events[1].Status)
	assert.Equal("in_progress", events[1].Run.Status)
	assert.Equal("updated", events[2].Status)
	assert.Equal("success", events[2].Run.Conclusion)
	for _, query := range provider.runQueries {
		assert.Equal("workflow_dispatch", query.Event)
		assert.Equal("main", query.Branch)
		assert.Equal("release.yml", query.WorkflowID)
	}
}

func TestWorkflowDispatchFollowThroughReportsUnresolvedRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &workflowTestProvider{
		caps:     platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		catalog:  []platform.WorkflowDefinition{workflowDefinitionFixture()},
		dispatch: platform.WorkflowDispatchResult{Accepted: true, Actor: "maintainer"},
	}
	provider.followRuns = func(int) platform.Page[platform.WorkflowRun] { return platform.Page[platform.WorkflowRun]{} }
	mux, _, handler := workflowFixtureWithRuntime(t, provider, httpapi.OperationAvailability{Available: true})

	status, body := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
	require.Equal(http.StatusAccepted, status)

	events := publishedDispatchEvents(handler)
	require.Len(events, 1)
	assert.Equal("unresolved", events[0].Status)
	assert.Equal(body["dispatch_id"], events[0].DispatchID)
	assert.Nil(events[0].Run)
	assert.Greater(len(provider.runQueries), 1, "keeps looking until the locate window closes")
}

func TestWorkflowDispatchFollowThroughSkipsLocateWhenProviderNamesRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &workflowTestProvider{
		caps:    platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()},
		dispatch: platform.WorkflowDispatchResult{Accepted: true, Actor: "maintainer", Run: &platform.WorkflowRun{
			ID: "run-9", WorkflowID: "release.yml", Status: "completed", Conclusion: "success",
		}},
	}
	mux, _, handler := workflowFixtureWithRuntime(t, provider, httpapi.OperationAvailability{Available: true})

	status, _ := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
	require.Equal(http.StatusAccepted, status)

	events := publishedDispatchEvents(handler)
	require.Len(events, 1)
	assert.Equal("located", events[0].Status)
	assert.Equal("run-9", events[0].Run.ID)
	assert.Empty(provider.runQueries, "a completed, named run needs no lookup or watch")
}

func TestWorkflowDispatchFollowThroughEnrichesPartialNamedRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	listed := platform.WorkflowRun{ID: "run-9", WorkflowID: "release.yml", RunNumber: 43, Name: "Release", Actor: "maintainer", Status: "in_progress", CreatedAt: time.Now()}
	provider := &workflowTestProvider{
		caps:    platform.Capabilities{ReadWorkflows: true, ReadWorkflowRuns: true, WorkflowDispatch: true},
		catalog: []platform.WorkflowDefinition{workflowDefinitionFixture()},
		dispatch: platform.WorkflowDispatchResult{Accepted: true, Actor: "maintainer", Run: &platform.WorkflowRun{
			ID: "run-9", WorkflowID: "release.yml", Actor: "maintainer",
		}},
	}
	provider.followRuns = func(call int) platform.Page[platform.WorkflowRun] {
		if call >= 2 {
			finished := listed
			finished.Status, finished.Conclusion = "completed", "success"
			return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{finished}}
		}
		return platform.Page[platform.WorkflowRun]{Items: []platform.WorkflowRun{listed}}
	}
	mux, _, handler := workflowFixtureWithRuntime(t, provider, httpapi.OperationAvailability{Available: true})

	status, _ := workflowRequest(t, mux, http.MethodPost, "/actions/github/acme/widget/workflows/release.yml/dispatch", map[string]any{"ref": "main", "expected_definition_sha": "definition-v1", "inputs": map[string]any{"version": "1"}})
	require.Equal(http.StatusAccepted, status)

	events := publishedDispatchEvents(handler)
	require.GreaterOrEqual(len(events), 2)
	assert.Equal("located", events[0].Status)
	assert.Equal(int64(43), events[0].Run.RunNumber)
	assert.Equal("in_progress", events[0].Run.Status)
	assert.Equal("updated", events[len(events)-1].Status)
	assert.Equal("success", events[len(events)-1].Run.Conclusion)
}
