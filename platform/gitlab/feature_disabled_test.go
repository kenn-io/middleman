package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func TestRepositoryFeatureError(t *testing.T) {
	metadataFailure := http.StatusInternalServerError
	tests := []struct {
		name                     string
		feature                  string
		operationErr             error
		issuesAccessLevel        string
		mergeRequestsAccessLevel string
		metadataStatus           int
		wantTarget               error
		wantOriginal             bool
		wantMetadataCalls        int
	}{
		{
			name: "forbidden disabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr:      &gitlab.ErrorResponse{StatusCode: http.StatusForbidden, Message: "feature failed"},
			issuesAccessLevel: "disabled", wantTarget: platform.ErrRepositoryFeatureDisabled,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "not found disabled merge requests", feature: platform.RepositoryFeatureMergeRequests,
			operationErr:             &gitlab.ErrorResponse{StatusCode: http.StatusNotFound, Message: "feature failed"},
			mergeRequestsAccessLevel: "disabled", wantTarget: platform.ErrRepositoryFeatureDisabled,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "gone disabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr:      &gitlab.ErrorResponse{StatusCode: http.StatusGone, Message: "feature failed"},
			issuesAccessLevel: "disabled", wantTarget: platform.ErrRepositoryFeatureDisabled,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "forbidden enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr:      &gitlab.ErrorResponse{StatusCode: http.StatusForbidden, Message: "feature failed"},
			issuesAccessLevel: "enabled", wantTarget: platform.ErrPermissionDenied,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "not found enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr:      &gitlab.ErrorResponse{StatusCode: http.StatusNotFound, Message: "feature failed"},
			issuesAccessLevel: "private", wantTarget: platform.ErrNotFound,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "gone enabled issues", feature: platform.RepositoryFeatureIssues,
			operationErr:      &gitlab.ErrorResponse{StatusCode: http.StatusGone, Message: "feature failed"},
			issuesAccessLevel: "public", wantTarget: platform.ErrProviderContract,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "unknown metadata", feature: platform.RepositoryFeatureIssues,
			operationErr: &gitlab.ErrorResponse{StatusCode: http.StatusNotFound, Message: "feature failed"},
			wantTarget:   platform.ErrNotFound, wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "metadata lookup failure", feature: platform.RepositoryFeatureIssues,
			operationErr:   &gitlab.ErrorResponse{StatusCode: http.StatusNotFound, Message: "feature failed"},
			metadataStatus: metadataFailure, wantTarget: platform.ErrNotFound,
			wantOriginal: true, wantMetadataCalls: 1,
		},
		{
			name: "unauthorized", feature: platform.RepositoryFeatureIssues,
			operationErr: &gitlab.ErrorResponse{StatusCode: http.StatusUnauthorized, Message: "feature failed"},
			wantTarget:   platform.ErrPermissionDenied,
		},
		{
			name: "rate limited", feature: platform.RepositoryFeatureIssues,
			operationErr: &gitlab.ErrorResponse{StatusCode: http.StatusTooManyRequests, Message: "feature failed"},
			wantTarget:   platform.ErrRateLimited,
		},
		{
			name: "server failure", feature: platform.RepositoryFeatureIssues,
			operationErr: &gitlab.ErrorResponse{StatusCode: http.StatusInternalServerError, Message: "feature failed"},
			wantTarget:   platform.ErrProviderContract, wantOriginal: true,
		},
		{
			name: "context canceled", feature: platform.RepositoryFeatureIssues,
			operationErr: context.Canceled, wantTarget: context.Canceled,
		},
		{
			name: "deadline exceeded", feature: platform.RepositoryFeatureIssues,
			operationErr: context.DeadlineExceeded, wantTarget: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadataCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadataCalls++
				assert.Equal(t, "/api/v4/projects/42", r.URL.EscapedPath())
				if tt.metadataStatus != 0 {
					http.Error(w, "metadata failed", tt.metadataStatus)
					return
				}
				writeJSON(w, fmt.Sprintf(`{
					"id":42,"path":"project","path_with_namespace":"group/project",
					"issues_access_level":%q,"merge_requests_access_level":%q
				}`, tt.issuesAccessLevel, tt.mergeRequestsAccessLevel))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			ref := platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.example.com",
				Owner: "group", Name: "project", RepoPath: "group/project", PlatformID: 42,
			}

			classified := client.repositoryFeatureError(
				t.Context(), ref, tt.feature, "test_read", tt.operationErr,
			)

			require := require.New(t)
			assert := assert.New(t)
			require.ErrorIs(classified, tt.wantTarget)
			if tt.wantOriginal {
				require.ErrorIs(classified, tt.operationErr)
			}
			assert.Equal(tt.wantMetadataCalls, metadataCalls)
			if errors.Is(classified, platform.ErrRepositoryFeatureDisabled) {
				var platformErr *platform.Error
				require.ErrorAs(classified, &platformErr)
				assert.Equal(platform.KindGitLab, platformErr.Provider)
				assert.Equal("gitlab.example.com", platformErr.PlatformHost)
				assert.Equal(tt.feature, platformErr.Capability)
			}
		})
	}
}

func TestClientItemLookupReusesFeatureMetadataConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	featureCalls := 0
	metadataCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/42/issues/7":
			featureCalls++
			http.Error(w, "issue missing", http.StatusNotFound)
		case "/api/v4/projects/42":
			metadataCalls++
			writeJSON(w, `{
				"id":42,"path":"project","path_with_namespace":"group/project",
				"issues_access_level":"enabled","merge_requests_access_level":"enabled"
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	ref := platform.RepoRef{
		Platform: platform.KindGitLab, Host: "gitlab.example.com",
		Owner: "group", Name: "project", RepoPath: "group/project", PlatformID: 42,
	}

	_, err := client.GetIssue(t.Context(), ref, 7)

	require.ErrorIs(err, platform.ErrLookupNotPresent)
	require.ErrorIs(err, platform.ErrNotFound)
	assert.Equal(1, featureCalls)
	assert.Equal(1, metadataCalls)
}

func TestClientClassifiesDisabledFeatureReads(t *testing.T) {
	tests := []struct {
		name         string
		feature      string
		featurePath  string
		status       int
		successPaths []string
		read         func(context.Context, *Client, platform.RepoRef) error
	}{
		{
			name: "open merge requests", feature: platform.RepositoryFeatureMergeRequests,
			featurePath: "/api/v4/projects/42/merge_requests", status: http.StatusForbidden,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListOpenMergeRequests(ctx, ref)
				return err
			},
		},
		{
			name: "merge request detail", feature: platform.RepositoryFeatureMergeRequests,
			featurePath: "/api/v4/projects/42/merge_requests/7", status: http.StatusNotFound,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.GetMergeRequest(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request discussions", feature: platform.RepositoryFeatureMergeRequests,
			featurePath: "/api/v4/projects/42/merge_requests/7/discussions", status: http.StatusGone,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListMergeRequestReviewThreads(ctx, ref, 7)
				return err
			},
		},
		{
			name: "merge request commits", feature: platform.RepositoryFeatureMergeRequests,
			featurePath: "/api/v4/projects/42/merge_requests/7/commits", status: http.StatusNotFound,
			successPaths: []string{"/api/v4/projects/42/merge_requests/7/discussions"},
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListMergeRequestEvents(ctx, ref, 7)
				return err
			},
		},
		{
			name: "open issues", feature: platform.RepositoryFeatureIssues,
			featurePath: "/api/v4/projects/42/issues", status: http.StatusForbidden,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListOpenIssues(ctx, ref)
				return err
			},
		},
		{
			name: "issue detail", feature: platform.RepositoryFeatureIssues,
			featurePath: "/api/v4/projects/42/issues/5", status: http.StatusNotFound,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.GetIssue(ctx, ref, 5)
				return err
			},
		},
		{
			name: "issue discussions", feature: platform.RepositoryFeatureIssues,
			featurePath: "/api/v4/projects/42/issues/5/discussions", status: http.StatusGone,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListIssueEvents(ctx, ref, 5)
				return err
			},
		},
		{
			name: "archive issues", feature: platform.RepositoryFeatureIssues,
			featurePath: "/api/v4/projects/42/issues", status: http.StatusNotFound,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				_, err := client.ListIssuesPage(ctx, ref, platform.ItemPageQuery{Order: platform.ItemOrderCreated})
				return err
			},
		},
		{
			name: "archive merge requests", feature: platform.RepositoryFeatureMergeRequests,
			featurePath: "/api/v4/projects/42/merge_requests", status: http.StatusForbidden,
			read: func(ctx context.Context, client *Client, ref platform.RepoRef) error {
				watermark := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
				_, err := client.ListMergeRequestsPage(ctx, ref, platform.ItemPageQuery{
					Order: platform.ItemOrderUpdated, UpdatedSince: &watermark,
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featureCalls := 0
			metadataCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.EscapedPath() {
				case tt.featurePath:
					featureCalls++
					http.Error(w, "feature failed", tt.status)
				case "/api/v4/projects/42":
					metadataCalls++
					issuesAccessLevel := "enabled"
					mergeRequestsAccessLevel := "enabled"
					if tt.feature == platform.RepositoryFeatureIssues {
						issuesAccessLevel = "disabled"
					} else {
						mergeRequestsAccessLevel = "disabled"
					}
					writeJSON(w, fmt.Sprintf(`{
						"id":42,"path":"project","path_with_namespace":"group/project",
						"issues_access_level":%q,"merge_requests_access_level":%q
					}`, issuesAccessLevel, mergeRequestsAccessLevel))
				default:
					if slices.Contains(tt.successPaths, r.URL.EscapedPath()) {
						writeJSON(w, `[]`)
						return
					}
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			ref := platform.RepoRef{
				Platform: platform.KindGitLab, Host: "gitlab.example.com",
				Owner: "group", Name: "project", RepoPath: "group/project", PlatformID: 42,
			}

			err := tt.read(t.Context(), client, ref)

			require := require.New(t)
			assert := assert.New(t)
			require.ErrorIs(err, platform.ErrRepositoryFeatureDisabled)
			var platformErr *platform.Error
			require.ErrorAs(err, &platformErr)
			assert.Equal(platform.KindGitLab, platformErr.Provider)
			assert.Equal("gitlab.example.com", platformErr.PlatformHost)
			assert.Equal(tt.feature, platformErr.Capability)
			assert.Equal(1, featureCalls)
			assert.Equal(1, metadataCalls)
		})
	}
}
