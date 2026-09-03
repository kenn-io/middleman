package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/runtimelock"
)

func TestArchiveCLIParseRepositoryRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    generated.ArchiveRepositoryRef
		wantErr string
	}{
		{
			name: "nested owner", value: "gitlab|gitlab.example/group/subgroup/project",
			want: generated.ArchiveRepositoryRef{
				Provider: "gitlab", PlatformHost: "gitlab.example", Owner: "group/subgroup",
				Name: "project", RepoPath: "group/subgroup/project",
			},
		},
		{name: "missing provider", value: "gitlab.example/owner/repo", wantErr: "provider|host/repo_path"},
		{name: "missing owner", value: "github|github.com/repo", wantErr: "provider|host/repo_path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			got, err := parseArchiveRepositoryRef(tt.value)
			if tt.wantErr != "" {
				require.Error(err)
				assert.Contains(err.Error(), tt.wantErr)
				return
			}
			require.NoError(err)
			assert.Equal(tt.want, got)
		})
	}
}

func TestArchiveCLIReportRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		name          string
		days          int
		start         string
		end           string
		wantStart     time.Time
		wantEnd       time.Time
		wantErrSubstr string
	}{
		{
			name: "rolling days", days: 2,
			wantStart: now.Add(-48 * time.Hour), wantEnd: now,
		},
		{
			name: "inclusive date end", start: "2026-07-01", end: "2026-07-03",
			wantStart: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "exact boundaries", start: "2026-07-01T01:00:00-05:00", end: "2026-07-02T06:00:00Z",
			wantStart: time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, 7, 2, 6, 0, 0, 0, time.UTC),
		},
		{name: "missing", wantErrSubstr: "--days or both --start and --end"},
		{name: "nonpositive days", days: -1, wantErrSubstr: "--days must be positive"},
		{name: "days and dates", days: 1, start: "2026-07-01", end: "2026-07-02", wantErrSubstr: "mutually exclusive"},
		{name: "mixed forms", start: "2026-07-01", end: "2026-07-02T00:00:00Z", wantErrSubstr: "same form"},
		{name: "inverted", start: "2026-07-03", end: "2026-07-01", wantErrSubstr: "start must precede end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			start, end, err := parseArchiveReportRange(now, tt.days, tt.start, tt.end)
			if tt.wantErrSubstr != "" {
				require.Error(err)
				assert.Contains(err.Error(), tt.wantErrSubstr)
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantStart, start)
			assert.Equal(tt.wantEnd, end)
		})
	}
}

func TestArchiveReportFromAPIPreservesTransportOrdering(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	activePhases := []string{"hydration", "prompt_maintenance"}
	activities := []generated.ArchiveReportActivityResponse{
		{
			Repository: archiveGeneratedCLIRef("github", "github.example", "owner", "repo"),
			Kind:       generated.ArchiveReportActivityResponseKind("review"), ItemNumber: 9,
			ProviderExternalId: "review-2", Author: "bob", Title: "Second",
			OccurredAt: start.Add(2 * time.Hour), Body: "body two", Url: "https://example.test/2",
		},
		{
			Repository: archiveGeneratedCLIRef("github", "github.example", "owner", "repo"),
			Kind:       generated.ArchiveReportActivityResponseKind("issue"), ItemNumber: 1,
			ProviderExternalId: "issue-1", Author: "alice", Title: "First",
			OccurredAt: start.Add(time.Hour), Body: "body one", Url: "https://example.test/1",
		},
	}
	transport := generated.ArchiveReportResponse{
		ReportSchema: report.Schema, Start: start, End: end,
		Repositories: []generated.ArchiveReportRepositoryResponse{{
			Repository: archiveGeneratedCLIRef("github", "github.example", "owner", "repo"),
			Coverage: generated.ArchiveReportCoverageResponse{
				Status: "current", ActivePhases: activePhases, CollectionMode: "full",
				OperatorState: "active", Comments: "supported", Reviews: "supported",
				InlineComments: "supported", ArchivedItems: 4,
			},
			Counts: generated.ArchiveReportCountsResponse{IssuesOpened: 1, ReviewsSubmitted: 1},
		}},
		Totals: generated.ArchiveReportCountsResponse{IssuesOpened: 1, ReviewsSubmitted: 1},
		Contributors: []generated.ArchiveReportContributorResponse{
			{Provider: "github", PlatformHost: "github.example", Login: "bob", Counts: generated.ArchiveReportCountsResponse{ReviewsSubmitted: 1}},
			{Provider: "github", PlatformHost: "github.example", Login: "alice", Counts: generated.ArchiveReportCountsResponse{IssuesOpened: 1}},
		},
		Activity: &activities,
	}

	model, err := archiveReportFromAPI(transport)
	require.NoError(err)
	require.Len(model.Repositories, 1)
	assert.Equal([]string{"hydration", "prompt_maintenance"}, model.Repositories[0].Coverage.ActivePhases)
	require.Len(model.Contributors, 2)
	assert.Equal("bob", model.Contributors[0].Login)
	assert.Equal("alice", model.Contributors[1].Login)
	require.Len(model.Activity, 2)
	assert.Equal(report.ActivityReview, model.Activity[0].Kind)
	assert.Equal("review-2", model.Activity[0].ProviderExternalID)
	assert.Equal(report.ActivityIssue, model.Activity[1].Kind)
	assert.Equal("https://example.test/1", model.Activity[1].URL)
}

func TestArchiveReportFromAPIPreservesLifecycleContract(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	actor := "merger"
	additions := int64(12)
	deletions := int64(4)
	filesChanged := int64(3)
	comments := int64(2)
	mergeCommitSHA := "abc123"
	activities := []generated.ArchiveReportActivityResponse{{
		Repository: archiveGeneratedCLIRef("github", "github.example", "owner", "repo"),
		Kind:       generated.ArchiveReportActivityResponseKind("merge_request_merged"),
		ItemNumber: 7, ProviderExternalId: "pr-7", Author: "author", Actor: &actor,
		Title: "Merged", OccurredAt: start.Add(time.Hour), Url: "https://example.test/7",
		Comments: &comments, Additions: &additions, Deletions: &deletions,
		FilesChanged: &filesChanged, MergeCommitSha: &mergeCommitSHA,
	}}
	transport := generated.ArchiveReportResponse{
		ReportSchema: report.Schema, Start: start, End: end,
		Repositories: []generated.ArchiveReportRepositoryResponse{{
			Repository: archiveGeneratedCLIRef("github", "github.example", "owner", "repo"),
			Coverage: generated.ArchiveReportCoverageResponse{
				Status: "current", CollectionMode: "full", OperatorState: "active",
				Issues: "supported", MergeRequests: "supported", Comments: "supported",
				Reviews: "supported", InlineComments: "supported",
			},
			Counts: generated.ArchiveReportCountsResponse{
				IssuesClosed: 1, MergeRequestsMerged: 1,
			},
		}},
		Totals: generated.ArchiveReportCountsResponse{
			IssuesClosed: 1, MergeRequestsMerged: 1,
		},
		Activity: &activities,
	}

	model, err := archiveReportFromAPI(transport)
	require.NoError(err)
	assert.Equal(report.Schema, model.Schema)
	require.Len(model.Repositories, 1)
	assert.Equal("supported", model.Repositories[0].Coverage.Issues)
	assert.Equal("supported", model.Repositories[0].Coverage.MergeRequests)
	assert.Equal(1, model.Repositories[0].Counts.IssuesClosed)
	assert.Equal(1, model.Repositories[0].Counts.MergeRequestsMerged)
	assert.Equal(1, model.Totals.IssuesClosed)
	assert.Equal(1, model.Totals.MergeRequestsMerged)
	require.Len(model.Activity, 1)
	activity := model.Activity[0]
	assert.Equal(report.ActivityMergeRequestMerged, activity.Kind)
	assert.Equal("merger", activity.Actor)
	assert.Equal(2, activity.Comments)
	assert.Equal(12, activity.Additions)
	assert.Equal(4, activity.Deletions)
	require.NotNil(activity.FilesChanged)
	assert.Equal(3, *activity.FilesChanged)
	assert.Equal("abc123", activity.MergeCommitSHA)
}

func TestArchiveReportFromAPIRejectsIncompatibleSchema(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		schema string
	}{
		{name: "missing", schema: ""},
		{name: "future", schema: "kenn-forge-archive-report/2"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := archiveReportFromAPI(generated.ArchiveReportResponse{
				ReportSchema: testCase.schema,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "archive report schema")
			assert.Contains(t, err.Error(), report.Schema)
		})
	}
}

func TestArchiveCLISubcommandsUseGeneratedDaemonContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)
	requests := make(chan *http.Request, 8)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/base/api/v1/archive/start", "/base/api/v1/archive/pause":
			_, _ = w.Write([]byte(`[{
				"repository":{"provider":"github","platform_host":"github.example","owner":"owner","name":"repo","repo_path":"owner/repo"},
				"collection_mode":"full","operator_state":"active","status":"running","active_phases":[],
				"counts":{"items":0,"complete_items":0,"pending_items":0,"failed_items":0,"unsupported_items":0,"inaccessible_items":0},
				"coverage":{"comments":"supported","reviews":"supported","inline_comments":"supported"}
			}]`))
		case "/base/api/v1/archive/status":
			_, _ = w.Write([]byte(`[]`))
		case "/base/api/v1/archive/report":
			_, _ = w.Write([]byte(`{
				"schema":"kenn-forge-archive-report/1",
				"start":"2026-07-11T15:30:00Z","end":"2026-07-13T15:30:00Z",
				"repositories":[],
				"totals":{"issues_opened":0,"issues_closed":0,"merge_requests_opened":0,"merge_requests_merged":1,"ordinary_comments":0,"reviews_submitted":0,"inline_review_comments":0},
				"contributors":[],
				"activity":[{
					"repository":{"provider":"github","platform_host":"github.example","owner":"owner","name":"repo","repo_path":"owner/repo"},
					"kind":"merge_request_merged","item_number":7,"provider_external_id":"pr-7",
					"title":"Merged","author":"author","actor":"merger",
					"occurred_at":"2026-07-12T15:30:00Z","body":"","url":"https://example.test/7",
					"comments":2,"additions":12,"deletions":4,"files_changed":3,"merge_commit_sha":"abc123"
				}]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	cfgPath := archiveCLITestConfig(t, api.URL, "/base", "archive-token")

	tests := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantOut    string
	}{
		{
			name: "start all", args: []string{"start", "--config", cfgPath, "--all"},
			wantMethod: http.MethodPost, wantPath: "/base/api/v1/archive/start", wantOut: `"status": "running"`,
		},
		{
			name: "pause repository", args: []string{"pause", "--config", cfgPath, "--repo", "github|github.example/owner/repo"},
			wantMethod: http.MethodPost, wantPath: "/base/api/v1/archive/pause", wantOut: `"repo_path": "owner/repo"`,
		},
		{
			name: "status filters", args: []string{"status", "--config", cfgPath,
				"--json", "--repo", "github|github.example/owner/one", "--repo", "gitlab|gitlab.example/group/two"},
			wantMethod: http.MethodGet, wantPath: "/base/api/v1/archive/status",
			wantQuery: url.Values{"repo": {"github|github.example/owner/one", "gitlab|gitlab.example/group/two"}},
			wantOut:   "[]",
		},
		{
			name: "report days json", args: []string{"report", "--config", cfgPath, "--days", "2", "--format", "json", "--verbose"},
			wantMethod: http.MethodGet, wantPath: "/base/api/v1/archive/report",
			wantQuery: url.Values{
				"start": {"2026-07-11T15:30:00Z"}, "end": {"2026-07-13T15:30:00Z"}, "verbose": {"true"},
			},
			wantOut: `"merge_commit_sha": "abc123"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			err := runArchiveCLIAt(tt.args, &stdout, func() time.Time { return now })
			require.NoError(err)
			assert.Contains(stdout.String(), tt.wantOut)
			req := <-requests
			assert.Equal(tt.wantMethod, req.Method)
			assert.Equal(tt.wantPath, req.URL.Path)
			assert.Equal("Bearer archive-token", req.Header.Get("Authorization"))
			if tt.wantQuery != nil {
				assert.Equal(tt.wantQuery, req.URL.Query())
			}
		})
	}
}

func TestArchiveAPIProblemIncludesStableDetails(t *testing.T) {
	t.Parallel()
	details := map[string]any{
		"provider": "gitlab", "platformHost": "gitlab.example", "capability": "submitted_reviews",
	}
	err := archiveAPIProblem("start archive", http.StatusBadRequest, &generated.ProblemError{
		Code: generated.ProblemErrorCode("unsupportedCapability"), Details: &details,
	})
	require.Error(t, err)
	assert.Equal(t,
		`start archive failed with HTTP 400 (unsupportedCapability; details={"capability":"submitted_reviews","platformHost":"gitlab.example","provider":"gitlab"})`,
		err.Error(),
	)
}

func TestArchiveCLIValidationAndAtomicOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{
			"code":"payloadTooLarge","detail":"unstable prose",
			"details":{"reason":"reportTooLarge","observedRecords":10001,"maxRecords":10000,"observedTextBytes":1,"maxTextBytes":2}
		}`))
	}))
	t.Cleanup(api.Close)
	cfgPath := archiveCLITestConfig(t, api.URL, "/", "")
	now := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)

	var stdout bytes.Buffer
	err := runArchiveCLIAt([]string{
		"report", "--config", cfgPath, "--days", "0",
	}, &stdout, func() time.Time { return now })
	require.Error(err)
	assert.Contains(err.Error(), "--days must be positive")
	assert.Zero(calls)

	err = runArchiveCLIAt([]string{
		"start", "--config", cfgPath, "--all", "--repo", "github|github.example/owner/repo",
	}, &stdout, func() time.Time { return now })
	require.Error(err)
	assert.Contains(err.Error(), "--all and --repo are mutually exclusive")
	assert.Zero(calls)
	assert.Empty(stdout.String())

	output := filepath.Join(t.TempDir(), "report.md")
	require.NoError(os.WriteFile(output, []byte("keep me\n"), 0o600))
	err = runArchiveCLIAt([]string{
		"report", "--config", cfgPath, "--days", "1", "--verbose", "--output", output,
	}, &stdout, func() time.Time { return now })
	require.Error(err)
	assert.Contains(err.Error(), "narrow the UTC range or repository scope")
	contents, readErr := os.ReadFile(output)
	require.NoError(readErr)
	assert.Equal("keep me\n", string(contents))
}

func TestArchiveCLIAtomicOutputSuccess(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"schema":"kenn-forge-archive-report/1",
			"start":"2026-07-12T15:30:00Z","end":"2026-07-13T15:30:00Z",
			"repositories":[],
			"totals":{"issues_opened":1,"merge_requests_opened":0,"ordinary_comments":0,"reviews_submitted":0,"inline_review_comments":0},
			"contributors":[]
		}`))
	}))
	t.Cleanup(api.Close)
	cfgPath := archiveCLITestConfig(t, api.URL, "/", "")
	output := filepath.Join(t.TempDir(), "report.md")
	require.NoError(os.WriteFile(output, []byte("old\n"), 0o600))
	var stdout bytes.Buffer
	err := runArchiveCLIAt([]string{
		"report", "--config", cfgPath, "--days", "1", "--output", output,
	}, &stdout, func() time.Time { return time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC) })
	require.NoError(err)
	assert.Empty(stdout.String())
	contents, err := os.ReadFile(output)
	require.NoError(err)
	assert.Contains(string(contents), "# Activity archive")
	assert.Contains(string(contents), "Issues opened: 1")
}

func archiveGeneratedCLIRef(provider, host, owner, name string) generated.ArchiveRepositoryRef {
	return generated.ArchiveRepositoryRef{
		Provider: provider, PlatformHost: host, Owner: owner, Name: name, RepoPath: owner + "/" + name,
	}
}

func archiveCLITestConfig(t *testing.T, rawURL, basePath, token string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	require.NoError(t, err)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	lock, err := runtimelock.Acquire(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	require.NoError(t, lock.WriteMetadata(runtimelock.Metadata{
		PID: os.Getpid(), ListenAddr: parsed.Host, BasePath: basePath,
	}))
	if token != "" {
		written, tokenErr := runtimelock.EnsureAuthToken(dataDir)
		require.NoError(t, tokenErr)
		if written != token {
			require.NoError(t, os.WriteFile(runtimelock.AuthTokenPath(dataDir), []byte(token+"\n"), 0o600))
		}
	}
	cfgPath := filepath.Join(root, "config.toml")
	configBody, err := json.Marshal(dataDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, []byte("data_dir = "+string(configBody)+"\n"), 0o600))
	return cfgPath
}
