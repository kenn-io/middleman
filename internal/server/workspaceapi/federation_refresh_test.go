package workspaceapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/platform"
)

func TestRefreshProviderWorkspaceFactsSyncsOnlyRequestedItem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		itemType       string
		number         int
		currentOwner   string
		currentName    string
		requestOwner   string
		requestName    string
		platformRepoID string
	}{
		{
			name: "pull request", itemType: db.WorkspaceItemTypePullRequest, number: 7,
			currentOwner: "acme", currentName: "widget",
			requestOwner: "acme", requestName: "widget",
		},
		{
			name: "issue", itemType: db.WorkspaceItemTypeIssue, number: 8,
			currentOwner: "acme", currentName: "widget",
			requestOwner: "acme", requestName: "widget",
		},
		{
			name:     "pull request after repository rename",
			itemType: db.WorkspaceItemTypePullRequest, number: 7,
			currentOwner: "renamed", currentName: "widget-next",
			requestOwner: "acme", requestName: "widget",
			platformRepoID: "repo-acme-widget",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			database := dbtest.Open(t)
			repoIdentity := db.RepoIdentity{
				Platform: string(platform.KindGitLab), PlatformHost: "git.example.test",
				PlatformRepoID: "repo-acme-widget",
				Owner:          test.currentOwner, Name: test.currentName,
			}
			repoID, err := database.UpsertRepo(t.Context(), repoIdentity)
			require.NoError(err)
			now := time.Now().UTC().Truncate(time.Second)
			providerRef := platform.RepoRef{
				Platform: platform.KindGitLab, Host: "git.example.test",
				Owner: test.currentOwner, Name: test.currentName,
				PlatformExternalID: "repo-acme-widget",
			}
			provider := &autoAssignProvider{
				pull: platform.MergeRequest{
					Repo: providerRef, PlatformID: 7, PlatformExternalID: "mr-7",
					Number: 7, URL: "https://git.example.test/acme/widget/merge_requests/7",
					Title: "Improve widget", State: "open", HeadBranch: "feature",
					BaseBranch: "main", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
				},
				issue: platform.Issue{
					Repo: providerRef, PlatformID: 8, PlatformExternalID: "issue-8",
					Number: 8, URL: "https://git.example.test/acme/widget/issues/8",
					Title: "Fix widget", State: "open",
					CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
				},
			}
			registry, err := platform.NewRegistry(provider)
			require.NoError(err)
			syncer := ghclient.NewSyncerWithRegistry(
				registry, database, nil, []ghclient.RepoRef{{
					Platform: platform.KindGitLab, RepoID: repoID,
					Owner: test.currentOwner, Name: test.currentName,
					PlatformHost:       "git.example.test",
					PlatformExternalID: "repo-acme-widget",
				}}, time.Hour, nil, nil,
			)
			t.Cleanup(syncer.Stop)
			handler := New(Deps{
				DB: database, Syncer: syncer,
				Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
			})

			err = handler.RefreshProviderWorkspaceFacts(t.Context(), providerplane.WorkspaceLaunchRequest{
				Repository: providerplane.RepositoryRoute{
					Provider: string(platform.KindGitLab), PlatformHost: "git.example.test",
					Owner: test.requestOwner, Name: test.requestName,
				},
				PlatformRepoID: test.platformRepoID,
				ItemType:       test.itemType, ItemNumber: test.number,
			})

			require.NoError(err)
			assert.Zero(provider.listPullCalls)
			assert.Zero(provider.listIssueCalls)
			if test.itemType == db.WorkspaceItemTypePullRequest {
				assert.Equal(1, provider.getPullCalls)
				assert.Zero(provider.getIssueCalls)
			} else {
				assert.Zero(provider.getPullCalls)
				assert.Equal(1, provider.getIssueCalls)
			}
		})
	}
}

func TestRefreshWorkspaceUsesHubProjectionWithoutLocalSyncer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	_, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	ws := &db.Workspace{
		ID: "ws-federated-refresh", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 7, ItemKey: "7",
		GitHeadRef: "feature/seven", WorkspaceBranch: "feature/seven",
		WorktreePath: filepath.Join(t.TempDir(), "worktree"), Status: "ready",
	}
	spec := workspaceLaunchSpecForRequest(providerplane.WorkspaceLaunchRequest{
		Repository: providerplane.RepositoryRoute{
			Provider: ws.Platform, PlatformHost: ws.PlatformHost,
			Owner: ws.RepoOwner, Name: ws.RepoName,
		},
		ItemType: ws.ItemType, ItemNumber: ws.ItemNumber,
		ItemKey: ws.ItemKey, GitHeadRef: ws.GitHeadRef,
	}, issuedAt)
	spec.SourceTitle = "Before refresh"
	require.NoError(database.CreateWorkspaceWithLaunchSpec(t.Context(), ws, spec))

	resolver := stubLaunchSpecResolver{refresh: func(
		_ context.Context, current db.WorkspaceLaunchSpec,
	) (db.WorkspaceLaunchSpec, error) {
		current.SourceTitle = "After refresh"
		current.IssuedAt = issuedAt.Add(time.Minute)
		current.SourceVisibleUntil = current.IssuedAt.Add(db.WorkspaceLaunchSpecVisibilityLease)
		return current, nil
	}}
	manager := workspace.NewManager(database, t.TempDir())
	handler := New(Deps{
		DB: database, Workspaces: manager, LaunchSpecResolver: resolver,
		Now: func() time.Time { return issuedAt.Add(time.Minute) },
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(handler.Shutdown(ctx))
	})

	output, err := handler.refreshWorkspace(
		t.Context(), &refreshWorkspaceInput{ID: ws.ID},
	)
	require.NoError(err)
	require.NotNil(output)
	refreshed, err := database.GetWorkspaceLaunchSpec(t.Context(), ws.ID)
	require.NoError(err)
	require.NotNil(refreshed)
	assert.Equal("After refresh", refreshed.SourceTitle)
}
