package syncertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/forgejo"
	"go.kenn.io/forge/platform/gitea"
)

type staticGiteaLikeToken struct {
	kind platform.Kind
	host string
}

func (s staticGiteaLikeToken) Token(context.Context) (string, error) { return "token", nil }

func (s staticGiteaLikeToken) Invalidate(string) {}

func (s staticGiteaLikeToken) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: string(s.kind), Host: s.host},
	}
}

func TestGiteaLikeProviderMergeMetricsReachArchiveReport(t *testing.T) {
	type newClient func(string, tokenauth.Source, string) (platform.MergeRequestReader, error)
	tests := []struct {
		name      string
		kind      platform.Kind
		host      string
		newClient newClient
	}{
		{
			name: "gitea", kind: platform.KindGitea, host: "gitea.example.com",
			newClient: func(host string, source tokenauth.Source, baseURL string) (platform.MergeRequestReader, error) {
				return gitea.NewClient(
					host, source,
					gitea.WithBaseURL(baseURL, true),
					gitea.WithServerVersion("1.26.0"), gitea.WithTransport(http.DefaultTransport))
			},
		},
		{
			name: "forgejo", kind: platform.KindForgejo, host: "forgejo.example.com",
			newClient: func(host string, source tokenauth.Source, baseURL string) (platform.MergeRequestReader, error) {
				return forgejo.NewClient(
					host, source, forgejo.WithBaseURLForTesting(baseURL), forgejo.WithTransport(http.DefaultTransport))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := t.Context()
			database := openTestDB(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/repos/group/project/pulls/8", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id": 1001, "number": 8,
					"title": "merged change", "state": "closed", "merged": true,
					"user": {"login": "author"}, "merged_by": {"login": "maintainer"},
					"html_url": "https://example.invalid/group/project/pulls/8",
					"head": {"ref": "feature", "sha": "head-sha"},
					"base": {"ref": "main", "sha": "base-sha"},
					"created_at": "2026-06-01T00:00:00Z",
					"updated_at": "2026-06-03T12:00:00Z",
					"merged_at": "2026-06-03T12:00:00Z",
					"closed_at": "2026-06-03T12:00:00Z",
					"additions": 21, "deletions": 5, "changed_files": 4
				}`))
			}))
			t.Cleanup(server.Close)

			client, err := tt.newClient(
				tt.host,
				staticGiteaLikeToken{kind: tt.kind, host: tt.host},
				server.URL,
			)
			require.NoError(err)
			ref := platform.RepoRef{
				Platform: tt.kind, Host: tt.host, Owner: "group", Name: "project",
				RepoPath: "group/project",
			}
			repoID, err := database.UpsertRepo(ctx, platformdb.DBRepoIdentity(ref))
			require.NoError(err)
			mergeRequest, err := client.GetMergeRequest(ctx, ref, 8)
			require.NoError(err)
			_, err = database.UpsertMergeRequest(
				ctx, platformdb.DBMergeRequest(repoID, mergeRequest),
			)
			require.NoError(err)

			tx, err := database.ReadDB().BeginTx(ctx, nil)
			require.NoError(err)
			activity, err := db.LoadArchiveReportActivity(
				ctx,
				tx,
				[]int64{repoID},
				time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC),
			)
			require.NoError(err)
			require.NoError(tx.Commit())
			var mergedActivity *db.ArchiveReportActivityRow
			for i := range activity {
				if activity[i].Kind == db.ArchiveReportActivityMergeRequestMerged {
					mergedActivity = &activity[i]
					break
				}
			}
			require.NotNil(mergedActivity)
			assert.Equal(21, mergedActivity.Additions)
			assert.Equal(5, mergedActivity.Deletions)
			require.NotNil(mergedActivity.FilesChanged)
			assert.Equal(4, *mergedActivity.FilesChanged)
		})
	}
}
