package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func pagesTestRef() platform.RepoRef {
	return platform.RepoRef{
		Platform: platform.KindGitHub, Host: "github.com",
		Owner: "acme", Name: "widget", RepoPath: "acme/widget",
	}
}

func TestGitHubRepositoryFeatureDisabled(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		status     int
		message    string
		want       bool
	}{
		{"issues disabled", platform.RepositoryFeatureIssues, http.StatusGone, "Issues are disabled for this repo", true},
		{"pull requests disabled", platform.RepositoryFeatureMergeRequests, http.StatusGone, "Pull Requests are disabled for this repo", true},
		{"unrelated gone", platform.RepositoryFeatureIssues, http.StatusGone, "Resource is gone", false},
		{"ambiguous forbidden", platform.RepositoryFeatureIssues, http.StatusForbidden, "Issues are disabled for this repo", false},
		{"ambiguous not found", platform.RepositoryFeatureIssues, http.StatusNotFound, "Issues are disabled for this repo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			err := &gh.ErrorResponse{
				Response: &http.Response{StatusCode: tt.status},
				Message:  tt.message,
			}
			classified := platformgithub.RepositoryFeatureDisabled("github.example.com", tt.capability, err)
			if !tt.want {
				assert.NoError(classified)
				return
			}
			require.ErrorIs(classified, platform.ErrRepositoryFeatureDisabled)
			var platformErr *platform.Error
			require.ErrorAs(classified, &platformErr)
			assert.Equal(tt.capability, platformErr.Capability)
			assert.Equal("github.example.com", platformErr.PlatformHost)
		})
	}
}

func TestGitHubArchiveMergeRequestInventoryClassifiesIssueOnlyRepository(t *testing.T) {
	tests := []struct {
		name               string
		repositoryStatus   int
		repositoryResponse string
		wantDisabled       bool
	}{
		{
			name:             "disabled",
			repositoryStatus: http.StatusOK,
			repositoryResponse: `{
				"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget",
				"owner":{"login":"acme"},"has_pull_requests":false
			}`,
			wantDisabled: true,
		},
		{
			name:             "enabled",
			repositoryStatus: http.StatusOK,
			repositoryResponse: `{
				"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget",
				"owner":{"login":"acme"},"has_pull_requests":true
			}`,
		},
		{
			name:             "unknown",
			repositoryStatus: http.StatusOK,
			repositoryResponse: `{
				"id":1,"node_id":"R_widget","name":"widget","full_name":"acme/widget",
				"owner":{"login":"acme"}
			}`,
		},
		{
			name:               "unreadable",
			repositoryStatus:   http.StatusForbidden,
			repositoryResponse: `{"message":"Forbidden"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			repositoryRequests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v3/repos/acme/widget/pulls":
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				case "/api/v3/repos/acme/widget":
					repositoryRequests++
					w.WriteHeader(tt.repositoryStatus)
					_, _ = w.Write([]byte(tt.repositoryResponse))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			provider := newArchiveTestGitHubProvider(t, srv.URL)
			_, err := provider.ListMergeRequestsPage(
				t.Context(), pagesTestRef(), platform.ItemPageQuery{Order: platform.ItemOrderCreated},
			)

			assert.Equal(1, repositoryRequests)
			if tt.wantDisabled {
				require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
				var platformErr *platform.Error
				require.ErrorAs(err, &platformErr)
				assert.Equal(platform.RepositoryFeatureMergeRequests, platformErr.Capability)
				return
			}
			require.Error(err)
			require.NotErrorIs(err, platform.ErrRepositoryFeatureDisabled)
			assert.Equal(http.StatusNotFound, platformgithub.StatusCode(err))
		})
	}
}

// requestRecorder captures the method, path, and raw query of every request a
// test server receives so parity tests can assert the canonical method and the
// legacy delegate issued identical requests.
type requestRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *requestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
}

func (r *requestRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.lines
	r.lines = nil
	return out
}
func TestGitHubLiveGetMapsLookupOutcomes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/repos/acme/widget/issues/9", "/api/v3/repos/acme/widget/pulls/9":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case "/api/v3/repos/acme/widget/issues/12":
			http.Error(w, `{"message":"This issue was deleted"}`, http.StatusGone)
		case "/api/v3/repos/acme/widget/issues/13":
			http.Error(w, `{"message":"Issues are disabled for this repo"}`, http.StatusGone)
		case "/api/v3/repos/acme/widget/issues/8", "/api/v3/repos/acme/widget/pulls/8":
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case "/api/v3/repos/acme/widget/issues/10":
			_, _ = w.Write([]byte(`{"id":10,"node_id":"I_10","number":10,"repository_url":"https://api.github.com/repos/other/place","title":"moved","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}`))
		case "/api/v3/repos/acme/widget/issues/11":
			_, _ = w.Write([]byte(`{"id":11,"node_id":"PR_11","number":11,"repository_url":"https://api.github.com/repos/acme/widget","title":"actually a pull request","pull_request":{"url":"https://api.github.com/repos/acme/widget/pulls/11"},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}`))
		case "/api/v3/repos/acme/widget/pulls/10":
			_, _ = w.Write([]byte(`{"id":10,"number":10,"title":"moved","state":"open","base":{"repo":{"url":"https://api.github.com/repos/other/place"}},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}`))
		case "/api/v3/repos/acme/widget":
			_, _ = w.Write([]byte(`{"id":1,"name":"widget","owner":{"login":"acme"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	provider := newArchiveTestGitHubProvider(t, srv.URL)
	ref := pagesTestRef()

	_, removedIssueErr := provider.GetIssue(t.Context(), ref, 9)
	require.ErrorIs(removedIssueErr, platform.ErrNotFound)
	require.ErrorIs(removedIssueErr, platform.ErrLookupNotPresent)
	_, deletedIssueErr := provider.GetIssue(t.Context(), ref, 12)
	require.ErrorIs(deletedIssueErr, platform.ErrNotFound)
	require.ErrorIs(deletedIssueErr, platform.ErrLookupNotPresent)
	_, disabledIssueErr := provider.GetIssue(t.Context(), ref, 13)
	require.ErrorIs(disabledIssueErr, platform.ErrRepositoryFeatureDisabled)
	require.NotErrorIs(disabledIssueErr, platform.ErrLookupNotPresent)
	_, removedMRErr := provider.GetMergeRequest(t.Context(), ref, 9)
	require.ErrorIs(removedMRErr, platform.ErrNotFound)
	require.ErrorIs(removedMRErr, platform.ErrLookupNotPresent)

	_, inaccessibleIssueErr := provider.GetIssue(t.Context(), ref, 8)
	require.ErrorIs(inaccessibleIssueErr, platform.ErrPermissionDenied)
	require.ErrorIs(inaccessibleIssueErr, platform.ErrLookupInaccessible)
	_, inaccessibleMRErr := provider.GetMergeRequest(t.Context(), ref, 8)
	require.ErrorIs(inaccessibleMRErr, platform.ErrPermissionDenied)
	require.ErrorIs(inaccessibleMRErr, platform.ErrLookupInaccessible)

	_, movedIssueErr := provider.GetIssue(t.Context(), ref, 10)
	require.ErrorIs(movedIssueErr, platform.ErrNotFound)
	require.ErrorIs(movedIssueErr, platform.ErrLookupNotPresent)
	var typedIssueErr *platform.Error
	require.ErrorAs(movedIssueErr, &typedIssueErr)
	require.NotNil(typedIssueErr.Destination)
	assert.Equal("other", typedIssueErr.Destination.Owner)
	assert.Equal("place", typedIssueErr.Destination.Name)

	_, movedMRErr := provider.GetMergeRequest(t.Context(), ref, 10)
	require.ErrorIs(movedMRErr, platform.ErrNotFound)
	require.ErrorIs(movedMRErr, platform.ErrLookupNotPresent)
	var typedMRErr *platform.Error
	require.ErrorAs(movedMRErr, &typedMRErr)
	require.NotNil(typedMRErr.Destination)
	assert.Equal("other", typedMRErr.Destination.Owner)
	assert.Equal("place", typedMRErr.Destination.Name)

	var typedRemovedErr *platform.Error
	require.ErrorAs(removedIssueErr, &typedRemovedErr)
	assert.Nil(typedRemovedErr.Destination)

	// An issue number that resolves to a pull request is not an issue:
	// the lookup must classify it removed instead of surfacing PR-flavored
	// issue data that downstream GraphQL issue reads can never resolve.
	_, prFlavoredErr := provider.GetIssue(t.Context(), ref, 11)
	require.ErrorIs(prFlavoredErr, platform.ErrNotFound)
	require.ErrorIs(prFlavoredErr, platform.ErrLookupNotPresent)
	var typedPRFlavoredErr *platform.Error
	require.ErrorAs(prFlavoredErr, &typedPRFlavoredErr)
	assert.Nil(typedPRFlavoredErr.Destination)
}

func TestGitHubArchiveDestinationIgnoresRepoCasing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ref := platform.RepoRef{
		Platform: platform.KindGitHub,
		Host:     "github.com",
		Owner:    "MixedOwner",
		Name:     "MixedName",
		RepoPath: "MixedOwner/MixedName",
	}

	assert.Nil(platformgithub.ArchiveDestination(
		ref,
		"https://api.github.com/repos/mixedowner/mixedname",
	))
	destination := platformgithub.ArchiveDestination(
		ref,
		"https://api.github.com/repos/OtherOwner/OtherName",
	)
	require.NotNil(destination)
	assert.Equal("otherowner", destination.Owner)
	assert.Equal("othername", destination.Name)
}

// TestGitHubUpdatedMergeRequestsAcrossPages proves the canonical maintenance
// query keeps records updated exactly at the inclusive
// watermark, and stop once the descending traversal crosses the overlapped
// watermark.
func TestGitHubUpdatedMergeRequestsAcrossPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	watermark := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recorder := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		assert.Equal("/api/v3/repos/acme/widget/pulls", r.URL.Path)
		assert.Equal("all", r.URL.Query().Get("state"))
		assert.Equal("updated", r.URL.Query().Get("sort"))
		assert.Equal("desc", r.URL.Query().Get("direction"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL(r), r.URL.Path))
			_, _ = w.Write([]byte(`[
				{"id":201,"node_id":"PR_201","number":41,"title":"newest","state":"closed","html_url":"https://github.com/acme/widget/pull/41","user":{"login":"a"},"created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-02T00:00:00Z"},
				{"id":202,"node_id":"PR_202","number":42,"title":"newer","state":"open","html_url":"https://github.com/acme/widget/pull/42","user":{"login":"a"},"created_at":"2025-01-02T00:00:00Z","updated_at":"2026-07-01T00:00:00Z"}
			]`))
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=3>; rel="next"`, srvURL(r), r.URL.Path))
			_, _ = w.Write([]byte(`[
				{"id":203,"node_id":"PR_203","number":43,"title":"at watermark","state":"closed","html_url":"https://github.com/acme/widget/pull/43","user":{"login":"a"},"created_at":"2025-01-03T00:00:00Z","updated_at":"2026-06-01T00:00:00Z"},
				{"id":204,"node_id":"PR_204","number":44,"title":"before watermark","state":"closed","html_url":"https://github.com/acme/widget/pull/44","user":{"login":"a"},"created_at":"2025-01-04T00:00:00Z","updated_at":"2026-05-01T00:00:00Z"}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	provider := newArchiveTestGitHubProvider(t, srv.URL)
	ref := pagesTestRef()

	canonicalFirst, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
		Order: platform.ItemOrderUpdated, UpdatedSince: &watermark,
	})
	require.NoError(err)
	require.NotEmpty(canonicalFirst.NextCursor)
	canonicalSecond, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
		Order:        platform.ItemOrderUpdated,
		UpdatedSince: &watermark, Cursor: canonicalFirst.NextCursor,
	})
	require.NoError(err)
	canonicalReqs := recorder.take()

	require.Len(canonicalFirst.Items, 2)
	assert.Equal([]int{41, 42}, []int{canonicalFirst.Items[0].Number, canonicalFirst.Items[1].Number})
	assert.False(canonicalFirst.Exhausted)
	// The record updated exactly at the watermark is inside the inclusive
	// boundary; the one before the overlapped watermark ends the scan.
	require.Len(canonicalSecond.Items, 1)
	assert.Equal(43, canonicalSecond.Items[0].Number)
	assert.True(canonicalSecond.Exhausted)
	assert.Empty(canonicalSecond.NextCursor)
	assert.Equal([]string{
		"GET /api/v3/repos/acme/widget/pulls?direction=desc&page=1&per_page=100&sort=updated&state=all",
		"GET /api/v3/repos/acme/widget/pulls?direction=desc&page=2&per_page=100&sort=updated&state=all",
	}, canonicalReqs)
}

func TestGitHubArchiveMergeRequestInventoryBypassesListETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("/api/v3/repos/acme/widget/pulls", r.URL.Path)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"archive-page"`)
		_, _ = w.Write([]byte(`[
			{"id":201,"node_id":"PR_201","number":41,"title":"archived","state":"closed","html_url":"https://github.com/acme/widget/pull/41","user":{"login":"a"},"created_at":"2025-01-01T00:00:00Z","updated_at":"2026-07-02T00:00:00Z"}
		]`))
	}))
	defer srv.Close()
	provider := newArchiveTestGitHubProvider(t, srv.URL)
	ref := pagesTestRef()

	for range 2 {
		page, err := provider.ListMergeRequestsPage(t.Context(), ref, platform.ItemPageQuery{
			Order: platform.ItemOrderUpdated,
		})
		require.NoError(err)
		require.Len(page.Items, 1)
		assert.Equal(41, page.Items[0].Number)
	}
	assert.Equal(2, requests)
}
