package pullapi

import (
	"net/http"
	"testing"
	"time"

	"go.kenn.io/forge/internal/platformdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

func newMidStackMergeFixture(
	t *testing.T,
) (*deferredMergeRouteServer, *deferredMergeTestProvider, *apiclient.Client) {
	t.Helper()
	ctx := t.Context()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		mergeRequests: []platform.MergeRequest{
			{
				Repo: ref, PlatformID: 7001, Number: 1, State: "open",
				HeadBranch: "bottom", BaseBranch: "main",
				HeadSHA: "bottom-head", BaseSHA: "base-sha",
			},
			{
				Repo: ref, PlatformID: 7002, Number: 2, State: "open",
				HeadBranch: "middle", BaseBranch: "bottom",
				HeadSHA: "middle-head", BaseSHA: "bottom-head",
			},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(ctx, platformdb.DBRepoIdentity(ref))
	require.NoError(t, err)
	bottomID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7001, Number: 1, State: db.MergeRequestStateOpen,
		Title: "Bottom", HeadBranch: "bottom", BaseBranch: "main",
		PlatformHeadSHA: "bottom-head", PlatformBaseSHA: "base-sha",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	middleID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7002, Number: 2, State: db.MergeRequestStateOpen,
		Title: "Middle", HeadBranch: "middle", BaseBranch: "bottom",
		PlatformHeadSHA: "middle-head", PlatformBaseSHA: "bottom-head",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(t, err)
	stackID, err := database.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(t, err)
	require.NoError(t, database.ReplaceStackMembers(ctx, stackID, []db.StackMember{
		{StackID: stackID, MergeRequestID: bottomID, Position: 1},
		{StackID: stackID, MergeRequestID: middleID, Position: 2},
	}))

	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform: platform.KindGitLab, PlatformHost: ref.Host,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}},
		time.Minute,
		nil,
		map[string]*ghclient.SyncBudget{
			ghclient.RateBucketKey("gitlab", ref.Host, "host"): ghclient.NewSyncBudget(100),
		},
	)
	t.Cleanup(syncer.Stop)
	server, client := newDeferredMergeHTTPFixture(t, database, syncer, now, 0)
	return server, provider, client
}

func TestMergePRRejectsMidStackMergeByDefault(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, provider, client := newMidStackMergeFixture(t)

	resp, err := client.HTTP.MergePullOnHostWithResponse(
		t.Context(),
		"gitlab.example.com",
		"gitlab",
		"group",
		"project",
		2,
		generated.MergePRInputBody{
			Method:          "squash",
			ExpectedHeadSha: new("middle-head"),
		},
	)
	require.NoError(err)
	assert.Equal(http.StatusConflict, resp.StatusCode())
	assert.Contains(string(resp.Body), `"reason":"mid_stack_merge_disallowed"`)
	assert.Contains(string(resp.Body), `"blocking_number":1`)
	assert.False(server.handler.allowMidStackMerges())
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestMergePRAllowsMidStackMergeFromCommittedConfigSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, provider, client := newMidStackMergeFixture(t)
	server.handler.ApplyConfig(ConfigSnapshot{AllowMidStackMerges: true})

	resp, err := client.HTTP.MergePullOnHostWithResponse(
		t.Context(),
		"gitlab.example.com",
		"gitlab",
		"group",
		"project",
		2,
		generated.MergePRInputBody{
			Method:          "squash",
			ExpectedHeadSha: new("middle-head"),
		},
	)
	require.NoError(err)
	assert.Equal(http.StatusOK, resp.StatusCode(), string(resp.Body))
	select {
	case call := <-provider.mergeCh:
		assert.Equal(2, call.Number)
		assert.Equal("middle-head", call.ExpectedHeadSHA)
	default:
		require.Fail("expected merge call")
	}
}
