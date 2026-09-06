package syncertest

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
	"go.kenn.io/forge/platform/gitlab"
)

// TestEssentialReserveKeepsDiscoveryAliveAfterOptionalExhaustion proves the
// discovery guarantee end to end over a real provider HTTP fixture, the real
// budget transport, and SQLite: after optional background spend has consumed
// the hourly ceiling, a newly opened merge request must still be discovered
// and persisted on the next sync cycle.
//
// The control variant runs the identical scenario on a budget without an
// essential reserve and must NOT discover the new merge request — proving
// the test discriminates the reserve behavior rather than passing vacuously.
func TestEssentialReserveKeepsDiscoveryAliveAfterOptionalExhaustion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	run := func(t *testing.T, budget *ghclient.SyncBudget) *string {
		t.Helper()
		ctx := t.Context()
		d := openTestDB(t)

		var newMRVisible atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.EscapedPath() {
			case "/api/v4/projects/group%2Fproject", "/api/v4/projects/42":
				_, _ = w.Write([]byte(`{
					"id": 42,
					"path": "project",
					"path_with_namespace": "group/project",
					"name": "Project",
					"default_branch": "main",
					"web_url": "https://gitlab.example.com/group/project",
					"http_url_to_repo": "https://gitlab.example.com/group/project.git",
					"created_at": "2026-01-01T00:00:00Z",
					"updated_at": "2026-01-02T00:00:00Z"
				}`))
			case "/api/v4/projects/42/merge_requests":
				seeded := `{
					"id": 1001, "iid": 8, "project_id": 42, "source_project_id": 42,
					"title": "seeded MR", "state": "opened",
					"author": {"username": "lin"},
					"source_branch": "feature", "target_branch": "main", "sha": "abc123",
					"web_url": "https://gitlab.example.com/group/project/-/merge_requests/8",
					"created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-02T00:00:00Z"
				}`
				if newMRVisible.Load() {
					_, _ = w.Write([]byte(`[` + seeded + `,{
						"id": 1002, "iid": 9, "project_id": 42, "source_project_id": 42,
						"title": "newly opened MR", "state": "opened",
						"author": {"username": "kai"},
						"source_branch": "feature-2", "target_branch": "main", "sha": "def456",
						"web_url": "https://gitlab.example.com/group/project/-/merge_requests/9",
						"created_at": "2026-06-03T00:00:00Z", "updated_at": "2026-06-03T00:00:00Z"
					}]`))
					return
				}
				_, _ = w.Write([]byte(`[` + seeded + `]`))
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		}))
		t.Cleanup(server.Close)

		client, err := gitlab.NewClient(
			"gitlab.example.com", staticGitLabToken("token"),
			gitlab.WithBaseURLForTesting(server.URL+"/api/v4"),
			gitlab.WithoutRetriesForTesting(),
			gitlab.WithTransport(ghclient.WrapSyncBudgetTransport(http.DefaultTransport, budget)))
		require.NoError(err)
		registry, err := ghclient.NewProviderRegistry(nil, client)
		require.NoError(err)
		repo := ghclient.RepoRef{
			Platform: platform.KindGitLab, PlatformHost: "gitlab.example.com",
			Owner: "group", Name: "project", RepoPath: "group/project",
		}
		syncer := ghclient.NewSyncerWithRegistry(
			registry, d, nil, []ghclient.RepoRef{repo}, time.Minute, nil,
			map[string]*ghclient.SyncBudget{
				ghclient.RateBucketKey("gitlab", "gitlab.example.com", "host"): budget,
			},
		)
		t.Cleanup(syncer.Stop)

		// Seed cycle with budget available: MR 8 lands.
		syncer.RunOnce(ctx)
		seededMR, err := d.GetMergeRequest(
			ctx, "gitlab", "gitlab.example.com", "group", "project", 8,
		)
		require.NoError(err)
		require.NotNil(seededMR, "seed cycle must persist the existing MR")

		// A new MR opens upstream while optional spend exhausts the ceiling.
		newMRVisible.Store(true)
		for budget.CanSpend(1) {
			budget.Spend(1)
		}

		syncer.RunOnce(ctx)

		newMR, err := d.GetMergeRequest(
			ctx, "gitlab", "gitlab.example.com", "group", "project", 9,
		)
		require.NoError(err)
		if newMR == nil {
			return nil
		}
		return &newMR.Title
	}

	t.Run("control without reserve stays starved", func(t *testing.T) {
		title := run(t, ghclient.NewSyncBudget(200))
		assert.Nil(title,
			"without a reserve the exhausted ceiling must block discovery — "+
				"a non-nil result means this test no longer discriminates")
	})

	t.Run("essential reserve discovers the new MR", func(t *testing.T) {
		title := run(t, ghclient.NewSyncBudgetWithEssentialReserve(200))
		require.NotNil(title,
			"discovery must survive optional budget exhaustion")
		assert.Equal("newly opened MR", *title)
	})
}
