package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/platform"
)

type workflowProviderFake struct {
	Client
	workflows      []*gh.Workflow
	definitions    map[string]string
	definitionErrs map[string]error
	environments   []*gh.Environment
	runs           platform.Page[*gh.WorkflowRun]
	jobs           []*gh.WorkflowJob
	dispatch       *gh.WorkflowDispatchRunDetails
	actor          string
	definitionRefs []string
	calls          []string
}

func (f *workflowProviderFake) GetRepository(context.Context, string, string) (*gh.Repository, error) {
	return &gh.Repository{Name: new("widgets"), DefaultBranch: new("trunk")}, nil
}
func (f *workflowProviderFake) AuthenticatedViewerLogin(context.Context) (string, error) {
	return f.actor, nil
}
func (f *workflowProviderFake) ListRepositoryWorkflows(context.Context, string, string) ([]*gh.Workflow, error) {
	f.calls = append(f.calls, "workflows")
	return f.workflows, nil
}
func (f *workflowProviderFake) GetWorkflowDefinition(_ context.Context, _, _, path, ref string) (string, string, error) {
	f.calls = append(f.calls, "definition:"+path+"@"+ref)
	f.definitionRefs = append(f.definitionRefs, ref)
	if err, ok := f.definitionErrs[path]; ok {
		return "", "", err
	}
	content, ok := f.definitions[path]
	if !ok {
		return "", "", errors.New("definition unavailable")
	}
	return content, "sha-" + path, nil
}
func (f *workflowProviderFake) ListRepositoryEnvironments(context.Context, string, string) ([]*gh.Environment, error) {
	f.calls = append(f.calls, "environments")
	return f.environments, nil
}
func (f *workflowProviderFake) ListManualWorkflowRuns(context.Context, string, string, int64, platform.WorkflowRunQuery) (platform.Page[*gh.WorkflowRun], error) {
	f.calls = append(f.calls, "runs")
	return f.runs, nil
}
func (f *workflowProviderFake) ListManualWorkflowJobs(context.Context, string, string, int64) ([]*gh.WorkflowJob, error) {
	f.calls = append(f.calls, "jobs")
	return f.jobs, nil
}
func (f *workflowProviderFake) DispatchManualWorkflow(context.Context, string, string, int64, gh.CreateWorkflowDispatchEventRequest) (*gh.WorkflowDispatchRunDetails, error) {
	f.calls = append(f.calls, "dispatch")
	return f.dispatch, nil
}

type workflowCatalogOnlyFake struct{ Client }

func (*workflowCatalogOnlyFake) ListRepositoryWorkflows(context.Context, string, string) ([]*gh.Workflow, error) {
	return nil, nil
}
func (*workflowCatalogOnlyFake) GetWorkflowDefinition(context.Context, string, string, string, string) (string, string, error) {
	return "", "", nil
}
func (*workflowCatalogOnlyFake) ListRepositoryEnvironments(context.Context, string, string) ([]*gh.Environment, error) {
	return nil, nil
}

type workflowRunOnlyFake struct{ Client }

func (*workflowRunOnlyFake) ListManualWorkflowRuns(context.Context, string, string, int64, platform.WorkflowRunQuery) (platform.Page[*gh.WorkflowRun], error) {
	return platform.Page[*gh.WorkflowRun]{}, nil
}
func (*workflowRunOnlyFake) ListManualWorkflowJobs(context.Context, string, string, int64) ([]*gh.WorkflowJob, error) {
	return nil, nil
}

type workflowDispatchOnlyFake struct{ Client }

func (*workflowDispatchOnlyFake) DispatchManualWorkflow(context.Context, string, string, int64, gh.CreateWorkflowDispatchEventRequest) (*gh.WorkflowDispatchRunDetails, error) {
	return nil, nil
}

func TestGitHubWorkflowCapabilitiesAreIndependent(t *testing.T) {
	tests := []struct {
		name        string
		client      Client
		readCatalog bool
		readRuns    bool
		dispatch    bool
	}{
		{name: "catalog", client: &workflowCatalogOnlyFake{}, readCatalog: true},
		{name: "runs", client: &workflowRunOnlyFake{}, readRuns: true},
		{name: "dispatch", client: &workflowDispatchOnlyFake{}, dispatch: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			provider := &gitHubClientProvider{host: "github.com", client: test.client}
			caps := provider.Capabilities()
			assert.Equal(test.readCatalog, caps.ReadWorkflows)
			assert.Equal(test.readRuns, caps.ReadWorkflowRuns)
			assert.Equal(test.dispatch, caps.WorkflowDispatch)
			if !test.readCatalog {
				_, err := provider.ListManualWorkflows(t.Context(), platform.RepoRef{})
				require.ErrorIs(err, platform.ErrUnsupportedCapability)
				_, err = provider.ListWorkflowEnvironments(t.Context(), platform.RepoRef{})
				require.ErrorIs(err, platform.ErrUnsupportedCapability)
			}
			if !test.readRuns {
				_, err := provider.ListWorkflowRuns(t.Context(), platform.RepoRef{}, platform.WorkflowRunQuery{})
				require.ErrorIs(err, platform.ErrUnsupportedCapability)
				_, err = provider.ListWorkflowRunJobs(t.Context(), platform.RepoRef{}, "1")
				require.ErrorIs(err, platform.ErrUnsupportedCapability)
			}
			if !test.dispatch {
				_, err := provider.DispatchWorkflow(t.Context(), platform.RepoRef{}, platform.WorkflowDispatchRequest{})
				require.ErrorIs(err, platform.ErrUnsupportedCapability)
			} else {
				result, err := provider.DispatchWorkflow(
					t.Context(),
					platform.RepoRef{Owner: "acme", Name: "widgets"},
					platform.WorkflowDispatchRequest{WorkflowID: "1", Ref: "main"},
				)
				require.NoError(err)
				assert.Empty(result.Actor)
			}
		})
	}
}

func TestGitHubWorkflowProviderCatalogPreservesPartialAvailability(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fake := &workflowProviderFake{
		workflows: []*gh.Workflow{
			{ID: new(int64(1)), Name: new("Release"), Path: new(".github/workflows/release.yml"), State: new("active"), HTMLURL: new("https://example.test/release")},
			{ID: new(int64(2)), Name: new("Broken"), Path: new(".github/workflows/broken.yml"), State: new("active")},
			{ID: new(int64(3)), Name: new("CI"), Path: new(".github/workflows/ci.yml"), State: new("active")},
			{ID: new(int64(5)), Name: new("Malformed"), Path: new(".github/workflows/malformed.yml"), State: new("active")},
			{ID: new(int64(4)), Name: new("Disabled"), Path: new("disabled.yml"), State: new("disabled_manually")},
		},
		definitions: map[string]string{
			".github/workflows/release.yml":   "on:\n  workflow_dispatch:\n    inputs:\n      target:\n        type: environment\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n",
			".github/workflows/ci.yml":        "on: [push]\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n",
			".github/workflows/malformed.yml": "on: workflow_dispatch\n---\non: workflow_dispatch\n",
		},
		environments: []*gh.Environment{{Name: new("production")}},
	}
	provider := &gitHubClientProvider{host: "github.com", client: fake}
	caps := provider.Capabilities()
	assert.True(caps.ReadWorkflows)
	assert.True(caps.ReadWorkflowRuns)
	assert.True(caps.WorkflowDispatch)

	catalog, err := provider.ListManualWorkflows(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"})
	require.NoError(err)
	require.Len(catalog, 3)
	assert.Equal("1", catalog[0].ID)
	assert.True(catalog[0].Available)
	assert.Equal("2", catalog[1].ID)
	assert.False(catalog[1].Available)
	assert.NotEmpty(catalog[1].UnavailableReason)
	assert.Equal("5", catalog[2].ID)
	assert.False(catalog[2].Available)
	assert.NotEmpty(catalog[2].UnavailableReason)
	assert.Equal([]string{"trunk", "trunk", "trunk", "trunk"}, fake.definitionRefs)

	environments, err := provider.ListWorkflowEnvironments(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"})
	require.NoError(err)
	assert.Equal([]platform.WorkflowEnvironment{{Name: "production"}}, environments)
}

func TestGitHubWorkflowProviderAbortsCatalogOnFatalDefinitionErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		wantIs error
	}{
		{name: "canceled", err: context.Canceled, wantIs: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, wantIs: context.DeadlineExceeded},
		{name: "typed rate limit", err: &platform.Error{Code: platform.ErrCodeRateLimited}, wantIs: platform.ErrRateLimited},
		{name: "GitHub rate limit", err: &gh.RateLimitError{Response: &http.Response{
			StatusCode: http.StatusForbidden,
			Request:    httptest.NewRequest(http.MethodGet, "https://api.github.com/repos/acme/widgets/contents/workflow.yml", nil),
		}}},
		{name: "unauthorized", err: &gh.ErrorResponse{
			Response: &http.Response{
				StatusCode: http.StatusUnauthorized,
				Request:    httptest.NewRequest(http.MethodGet, "https://api.github.com/repos/acme/widgets/contents/workflow.yml", nil),
			},
			Message: "bad credentials",
		}},
		{name: "server failure", err: &gh.ErrorResponse{
			Response: &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Request:    httptest.NewRequest(http.MethodGet, "https://api.github.com/repos/acme/widgets/contents/workflow.yml", nil),
			},
			Message: "service unavailable",
		}},
		{name: "transport", err: &url.Error{
			Op:  "Get",
			URL: "https://api.github.com/repos/acme/widgets/contents/workflow.yml",
			Err: errors.New("connection reset"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			fake := &workflowProviderFake{
				workflows: []*gh.Workflow{
					{ID: new(int64(1)), Name: new("Blocked"), Path: new(".github/workflows/blocked.yml"), State: new("active")},
					{ID: new(int64(2)), Name: new("Release"), Path: new(".github/workflows/release.yml"), State: new("active")},
				},
				definitions: map[string]string{
					".github/workflows/release.yml": "on: workflow_dispatch\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n",
				},
				definitionErrs: map[string]error{
					".github/workflows/blocked.yml": test.err,
				},
			}
			provider := &gitHubClientProvider{host: "github.com", client: fake}
			_, err := provider.ListManualWorkflows(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"})
			require.Error(t, err)
			if test.wantIs != nil {
				require.ErrorIs(t, err, test.wantIs)
			} else {
				assert.Same(test.err, err)
			}
			assert.Equal([]string{
				"workflows",
				"definition:.github/workflows/blocked.yml@trunk",
			}, fake.calls)
			assert.Equal([]string{"trunk"}, fake.definitionRefs)
		})
	}
}

func TestGitHubWorkflowProviderKeepsPerDefinitionFailuresPartial(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "not found",
			err: &gh.ErrorResponse{
				Response: &http.Response{StatusCode: http.StatusNotFound},
				Message:  "workflow file not found",
			},
		},
		{name: "decode", err: errors.New("decode workflow definition: invalid base64")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			fake := &workflowProviderFake{
				workflows: []*gh.Workflow{
					{ID: new(int64(1)), Name: new("Broken"), Path: new(".github/workflows/broken.yml"), State: new("active")},
					{ID: new(int64(2)), Name: new("Release"), Path: new(".github/workflows/release.yml"), State: new("active")},
				},
				definitions: map[string]string{
					".github/workflows/release.yml": "on: workflow_dispatch\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n",
				},
				definitionErrs: map[string]error{
					".github/workflows/broken.yml": test.err,
				},
			}
			provider := &gitHubClientProvider{host: "github.com", client: fake}
			definitions, err := provider.ListManualWorkflows(
				t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"},
			)
			require.NoError(t, err)
			require.Len(t, definitions, 2)
			assert.False(definitions[0].Available)
			assert.True(definitions[1].Available)
			assert.Equal([]string{
				"workflows",
				"definition:.github/workflows/broken.yml@trunk",
				"definition:.github/workflows/release.yml@trunk",
			}, fake.calls)
		})
	}
}

func TestGitHubWorkflowEnvironmentsReadOnlyEnvironmentTransport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fake := &workflowProviderFake{
		environments: []*gh.Environment{{Name: new("production")}},
	}
	provider := &gitHubClientProvider{host: "github.com", client: fake}
	environments, err := provider.ListWorkflowEnvironments(
		t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"},
	)
	require.NoError(err)
	assert.Equal([]platform.WorkflowEnvironment{{Name: "production"}}, environments)
	assert.Equal([]string{"environments"}, fake.calls)
	assert.Empty(fake.definitionRefs)
}

func TestGitHubWorkflowProviderNormalizesRunsJobsAndDispatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	created, err := time.Parse(time.RFC3339, "2026-08-27T12:00:00+01:00")
	require.NoError(err)
	updated := created.Add(time.Minute)
	started := created.Add(time.Second)
	completed := created.Add(2 * time.Second)
	fake := &workflowProviderFake{
		actor: "maintainer",
		runs: platform.Page[*gh.WorkflowRun]{NextCursor: "3", Exhausted: true, Items: []*gh.WorkflowRun{{
			ID: new(int64(100)), WorkflowID: new(int64(42)), RunNumber: new(7), Name: new("Release"),
			Event: new("workflow_dispatch"), HeadBranch: new("main"), HeadSHA: new("abc"), Actor: &gh.User{Login: new("octocat")},
			Status: new("completed"), Conclusion: new("success"), CreatedAt: &gh.Timestamp{Time: created}, UpdatedAt: &gh.Timestamp{Time: updated}, HTMLURL: new("https://example.test/runs/100"),
		}}},
		jobs:     []*gh.WorkflowJob{{ID: new(int64(9)), Name: new("deploy"), Status: new("completed"), Conclusion: new("success"), StartedAt: &gh.Timestamp{Time: started}, CompletedAt: &gh.Timestamp{Time: completed}, HTMLURL: new("https://example.test/jobs/9"), Steps: []*gh.TaskStep{{Number: new(int64(1)), Name: new("ship"), Status: new("completed"), Conclusion: new("success"), StartedAt: &gh.Timestamp{Time: started}, CompletedAt: &gh.Timestamp{Time: completed}}}}},
		dispatch: &gh.WorkflowDispatchRunDetails{WorkflowRunID: new(int64(101)), HTMLURL: new("https://example.test/runs/101")},
	}
	provider := &gitHubClientProvider{host: "github.com", client: fake}
	page, err := provider.ListWorkflowRuns(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"}, platform.WorkflowRunQuery{WorkflowID: "42"})
	require.NoError(err)
	assert.Equal(platform.Page[platform.WorkflowRun]{
		NextCursor: "3", Exhausted: true,
		Items: []platform.WorkflowRun{{
			ID: "100", WorkflowID: "42", RunNumber: 7, Name: "Release",
			Event: "workflow_dispatch", Ref: "main", HeadSHA: "abc", Actor: "octocat",
			Status: "completed", Conclusion: "success", CreatedAt: created.UTC(),
			UpdatedAt: updated.UTC(), WebURL: "https://example.test/runs/100",
		}},
	}, page)
	jobs, err := provider.ListWorkflowRunJobs(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"}, "100")
	require.NoError(err)
	assert.Equal([]platform.WorkflowRunJob{{
		ID: "9", Name: "deploy", Status: "completed", Conclusion: "success",
		StartedAt: started.UTC(), CompletedAt: completed.UTC(),
		WebURL: "https://example.test/jobs/9",
		Steps: []platform.WorkflowRunStep{{
			Number: 1, Name: "ship", Status: "completed", Conclusion: "success",
			StartedAt: started.UTC(), CompletedAt: completed.UTC(),
		}},
	}}, jobs)
	result, err := provider.DispatchWorkflow(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"}, platform.WorkflowDispatchRequest{WorkflowID: "42", Ref: "main"})
	require.NoError(err)
	assert.Equal(platform.WorkflowDispatchResult{
		Actor:    "maintainer",
		Accepted: true,
		Run: &platform.WorkflowRun{
			ID: "101", WorkflowID: "42", Actor: "maintainer",
			WebURL: "https://example.test/runs/101",
		},
	}, result)
	fake.dispatch = nil
	result, err = provider.DispatchWorkflow(t.Context(), platform.RepoRef{Owner: "acme", Name: "widgets"}, platform.WorkflowDispatchRequest{WorkflowID: "42", Ref: "main"})
	require.NoError(err)
	assert.Equal(platform.WorkflowDispatchResult{Accepted: true, LocatingRun: true, Actor: "maintainer"}, result)
	_, err = provider.DispatchWorkflow(t.Context(), platform.RepoRef{}, platform.WorkflowDispatchRequest{WorkflowID: "not-decimal"})
	require.ErrorIs(err, platform.ErrInvalidArgument)
}

func TestGitHubWorkflowProviderUnsupportedClientsAreTyped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	provider := &gitHubClientProvider{host: "github.com", client: &mockClient{}}
	assert.False(provider.Capabilities().ReadWorkflows)
	_, err := provider.ListManualWorkflows(t.Context(), platform.RepoRef{})
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = provider.ListWorkflowRuns(t.Context(), platform.RepoRef{}, platform.WorkflowRunQuery{})
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = provider.DispatchWorkflow(t.Context(), platform.RepoRef{}, platform.WorkflowDispatchRequest{})
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
}

func TestRoutedClientRoutesWorkflowOperationsByRepository(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fallback := &workflowProviderFake{}
	exact := &workflowProviderFake{
		definitions: map[string]string{"release.yml": "on: workflow_dispatch\njobs:\n  noop:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n"},
		dispatch:    &gh.WorkflowDispatchRunDetails{WorkflowRunID: new(int64(8))},
	}
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: fallback},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme", Name: "widgets"}, Client: exact},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)
	_, err = routed.ListRepositoryWorkflows(t.Context(), "acme", "widgets")
	require.NoError(err)
	_, _, err = routed.GetWorkflowDefinition(t.Context(), "acme", "widgets", "release.yml", "main")
	require.NoError(err)
	_, err = routed.ListRepositoryEnvironments(t.Context(), "acme", "widgets")
	require.NoError(err)
	_, err = routed.ListManualWorkflowRuns(t.Context(), "acme", "widgets", 42, platform.WorkflowRunQuery{})
	require.NoError(err)
	_, err = routed.ListManualWorkflowJobs(t.Context(), "acme", "widgets", 99)
	require.NoError(err)
	_, err = routed.DispatchManualWorkflow(t.Context(), "acme", "widgets", 42, gh.CreateWorkflowDispatchEventRequest{Ref: "main"})
	require.NoError(err)
	assert.Equal([]string{
		"workflows", "definition:release.yml@main", "environments", "runs", "jobs", "dispatch",
	}, exact.calls)
	assert.Empty(fallback.calls)
}

func TestRoutedClientWorkflowMethodsRejectClientsWithoutOptionalInterfaces(t *testing.T) {
	require := require.New(t)
	router, err := NewHostRouter(
		"github.com",
		&Route{Key: RouteKey{Host: "github.com"}, Client: &mockClient{}},
		&Route{Key: RouteKey{Host: "github.com", Owner: "acme", Name: "widgets"}, Client: &mockClient{}},
	)
	require.NoError(err)
	routed, err := NewRoutedClient(router)
	require.NoError(err)

	_, err = routed.ListRepositoryWorkflows(t.Context(), "acme", "widgets")
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, _, err = routed.GetWorkflowDefinition(t.Context(), "acme", "widgets", "release.yml", "main")
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = routed.ListRepositoryEnvironments(t.Context(), "acme", "widgets")
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = routed.ListManualWorkflowRuns(t.Context(), "acme", "widgets", 42, platform.WorkflowRunQuery{})
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = routed.ListManualWorkflowJobs(t.Context(), "acme", "widgets", 99)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
	_, err = routed.DispatchManualWorkflow(
		t.Context(), "acme", "widgets", 42, gh.CreateWorkflowDispatchEventRequest{Ref: "main"},
	)
	require.ErrorIs(err, platform.ErrUnsupportedCapability)
}
