package workspaceapi

import (
	"context"
	"errors"
	"fmt"
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
	"go.kenn.io/forge/internal/workspace/localruntime"
	"go.kenn.io/forge/platform"
)

type recordingWorkspaceAutomation struct {
	requests []ProviderWorkspaceItemRequest
}

func TestCreateAdHocWorkspaceResolvesMissingRepositoryBeforeLocalCreate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	manager := workspace.NewManager(database, t.TempDir())
	resolved := false
	handler := New(Deps{
		DB: database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{
			DB: database,
		}),
		Workspaces: manager,
		ResolveRepository: func(
			ctx context.Context, route providerplane.RepositoryRoute,
		) (*db.Repo, error) {
			resolved = true
			assert.Equal(providerplane.RepositoryRoute{
				Provider: "github", PlatformHost: "github.com",
				Owner: "acme", Name: "widget",
			}, route)
			entry, _, err := database.ReconcileRepositoryObservation(ctx, db.RepoIdentity{
				Platform: route.Provider, PlatformHost: route.PlatformHost,
				PlatformRepoID: "stable-provider-id",
				Owner:          route.Owner, Name: route.Name,
			}, time.Now().UTC())
			if err != nil {
				return nil, err
			}
			return &entry.Repository, nil
		},
		EnrichmentDisabled: true,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(handler.Shutdown(ctx))
	})
	branch := "work/remote-create"

	result, err := handler.CreateAdHocWorkspaceService(
		t.Context(), CreateAdHocWorkspaceRequest{
			Provider: "github", PlatformHost: "github.com",
			Owner: "acme", Name: "widget", Branch: &branch,
		},
	)

	require.NoError(err)
	assert.True(resolved)
	assert.NotEmpty(result.Workspace.ID)
	entry, err := database.GetRepositoryByProviderID(
		t.Context(), "github", "github.com", "stable-provider-id",
	)
	require.NoError(err)
	require.NotNil(entry)
	assert.Equal("acme", entry.Repository.Owner)
	assert.Equal("widget", entry.Repository.Name)
}

func (a *recordingWorkspaceAutomation) AutoAssignWorkspaceItem(
	_ context.Context, request ProviderWorkspaceItemRequest,
) error {
	a.requests = append(a.requests, request)
	return nil
}

func TestLaunchSpecCreatePersistsBeforeSetupStarts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	_, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	})
	require.NoError(err)
	resolver := stubLaunchSpecResolver{}
	manager := workspace.NewManager(database, t.TempDir())
	manager.SetLaunchSpecResolver(resolver)
	type setupObservation struct {
		workspaceID string
		spec        *db.WorkspaceLaunchSpec
		err         error
	}
	setupStarted := make(chan setupObservation, 1)
	manager.SetWorktreeBasePathResolver(func(
		ctx context.Context, _ workspace.WorktreeBaseRepository,
	) (string, bool, error) {
		rows, readErr := database.ListWorkspaces(ctx)
		if readErr != nil || len(rows) != 1 {
			setupStarted <- setupObservation{err: readErr}
			return "", false, errors.New("stop setup after launch-spec observation")
		}
		spec, readErr := database.GetWorkspaceLaunchSpec(ctx, rows[0].ID)
		setupStarted <- setupObservation{
			workspaceID: rows[0].ID, spec: spec, err: readErr,
		}
		return "", false, errors.New("stop setup after launch-spec observation")
	})
	handler := New(Deps{
		DB: database, Workspaces: manager,
		LaunchSpecResolver: resolver, EnrichmentDisabled: true,
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(handler.Shutdown(shutdownCtx))
	})

	result, err := handler.CreatePullWorkspace(t.Context(), CreatePullWorkspaceRequest{
		Provider: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "widget", Number: 42,
	})
	require.NoError(err)

	select {
	case observed := <-setupStarted:
		require.NoError(observed.err)
		assert.Equal(result.Workspace.ID, observed.workspaceID)
		require.NotNil(observed.spec)
		assert.Equal(42, observed.spec.ItemNumber)
	case <-time.After(5 * time.Second):
		require.FailNow("workspace setup did not start")
	}
}

func TestCreatePullWorkspacePreservesDisplacedRouteOwner(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	base := t.TempDir()
	observedAt := time.Now().UTC().Add(-3 * time.Minute)
	oldIdentity := db.RepoIdentity{
		Platform: "github", PlatformHost: "github.com",
		PlatformRepoID: "repo-old-widget", Owner: "acme", Name: "widget",
	}
	oldRepo, accepted, err := database.ReconcileRepositoryObservation(
		t.Context(), oldIdentity, observedAt,
	)
	require.NoError(err)
	require.True(accepted)
	require.NotNil(oldRepo)
	displacedPath := filepath.Join(
		base, "github", "github.com", "acme", "widget",
		fmt.Sprintf("repo-%d", oldRepo.Repository.ID), "pr-42",
	)
	require.NoError(database.InsertWorkspace(t.Context(), &db.Workspace{
		ID: "ws-displaced", Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42, ItemKey: "42",
		GitHeadRef: "feature/old", WorkspaceBranch: "kenn-forge/pr-42",
		WorktreePath: displacedPath, TmuxSession: "forge-ws-displaced", Status: "ready",
	}))
	oldIdentity.Owner = "acme-archive"
	oldIdentity.Name = "widget-old"
	_, accepted, err = database.ReconcileRepositoryObservation(
		t.Context(), oldIdentity, observedAt.Add(time.Minute),
	)
	require.NoError(err)
	require.True(accepted)
	_, accepted, err = database.ReconcileRepositoryObservation(
		t.Context(), db.RepoIdentity{
			Platform: "github", PlatformHost: "github.com",
			PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
		}, observedAt.Add(2*time.Minute),
	)
	require.NoError(err)
	require.True(accepted)

	resolver := stubLaunchSpecResolver{}
	manager := workspace.NewManager(database, base)
	manager.SetLaunchSpecResolver(resolver)
	handler := New(Deps{
		DB: database, Workspaces: manager,
		LaunchSpecResolver: resolver, EnrichmentDisabled: true,
	})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	require.NoError(handler.Shutdown(shutdownCtx))
	cancel()

	result, err := handler.CreatePullWorkspace(t.Context(), CreatePullWorkspaceRequest{
		Provider: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "widget", Number: 42,
	})

	require.NoError(err)
	assert.NotEqual("ws-displaced", result.Workspace.ID)
	preserved, err := database.GetWorkspace(t.Context(), "ws-displaced")
	require.NoError(err)
	require.NotNil(preserved)
	assert.Equal(displacedPath, preserved.WorktreePath)
	replacement, err := database.GetWorkspace(t.Context(), result.Workspace.ID)
	require.NoError(err)
	require.NotNil(replacement)
	assert.NotEqual(displacedPath, replacement.WorktreePath)
	assert.Equal("pr-42", filepath.Base(replacement.WorktreePath))
}

func TestCreatePullWorkspaceServiceSuppressesAutoAssign(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	ctx := t.Context()
	repoIdentity := db.RepoIdentity{
		Platform: "gitlab", PlatformHost: "git.example.test",
		PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
	}
	repoID, err := database.UpsertRepo(ctx, repoIdentity)
	require.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID: repoID, PlatformID: 7000, Number: 7,
		URL:   "https://git.example.test/acme/widget/merge_requests/7",
		Title: "Improve widget", Author: "author", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main",
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)
	provider := &autoAssignProvider{pull: platform.MergeRequest{}}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Hour, nil, nil)
	t.Cleanup(syncer.Stop)
	launchSpecs := stubLaunchSpecResolver{}
	workspaceManager := workspace.NewManager(database, t.TempDir())
	workspaceManager.SetLaunchSpecResolver(launchSpecs)
	handler := New(Deps{
		DB:       database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
		Syncer:   syncer, Config: ConfigSnapshot{AutoAssignOnCreate: true},
		Workspaces:         workspaceManager,
		LaunchSpecResolver: launchSpecs,
		EnrichmentDisabled: true,
	})
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(handler.Shutdown(shutdownCtx))
	})

	result, err := handler.CreatePullWorkspace(ctx, CreatePullWorkspaceRequest{
		Provider: "gitlab", PlatformHost: "git.example.test",
		Owner: "acme", Name: "widget", Number: 7, SuppressAutoAssign: true,
	})

	require.NoError(err)
	assert.NotEmpty(result.Workspace.ID)
	assert.True(result.Workspace.Created)
	assert.Empty(provider.pullAssigned)
}

func TestFederationWorkspaceCreationUsesHubAutoAssignment(t *testing.T) {
	tests := []struct {
		name     string
		itemType string
		number   int
		create   func(*Handler) error
	}{
		{
			name: "pull request", itemType: db.WorkspaceItemTypePullRequest, number: 7,
			create: func(handler *Handler) error {
				_, err := handler.CreatePullWorkspace(t.Context(), CreatePullWorkspaceRequest{
					Provider: "github", PlatformHost: "github.com",
					Owner: "acme", Name: "widget", Number: 7,
				})
				return err
			},
		},
		{
			name: "issue", itemType: db.WorkspaceItemTypeIssue, number: 8,
			create: func(handler *Handler) error {
				_, err := handler.CreateIssueWorkspaceService(t.Context(), CreateIssueWorkspaceRequest{
					Provider: "github", PlatformHost: "github.com",
					Owner: "acme", Name: "widget", Number: 8,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			database := dbtest.Open(t)
			_, err := database.UpsertRepo(t.Context(), db.RepoIdentity{
				Platform: "github", PlatformHost: "github.com",
				PlatformRepoID: "repo-acme-widget", Owner: "acme", Name: "widget",
			})
			require.NoError(err)
			resolver := stubLaunchSpecResolver{}
			manager := workspace.NewManager(database, t.TempDir())
			automation := &recordingWorkspaceAutomation{}
			handler := New(Deps{
				DB: database, Workspaces: manager, LaunchSpecResolver: resolver,
				ProviderWorkspaceAutomation: automation,
				Config:                      ConfigSnapshot{AutoAssignOnCreate: true},
				EnrichmentDisabled:          true,
			})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(handler.Shutdown(ctx))
			})

			require.NoError(test.create(handler))
			require.Len(automation.requests, 1)
			assert.Equal(test.itemType, automation.requests[0].ItemType)
			assert.Equal(test.number, automation.requests[0].ItemNumber)
		})
	}
}

func TestLaunchWorkspaceRuntimeServiceReturnsSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	ctx := t.Context()
	worktree := t.TempDir()
	workspaceID := "ws-runtime-service"
	require.NoError(database.InsertWorkspace(ctx, &db.Workspace{
		ID: workspaceID, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widget", ItemType: db.WorkspaceItemTypeAdHoc,
		ItemKey: db.AdHocWorkspaceItemKey("work/service"), GitHeadRef: "work/service",
		WorkspaceBranch: "work/service", WorktreePath: worktree,
		TmuxSession: "forge-runtime-service", Status: "ready",
	}))
	owner := newInitialMessagePTYOwner()
	runtime := localruntime.NewManager(localruntime.Options{
		Targets: []localruntime.LaunchTarget{{
			Key: string(localruntime.LaunchTargetPlainShell), Label: "Shell",
			Kind: localruntime.LaunchTargetPlainShell, Source: "system", Available: true,
		}},
		PtyOwnerRuntime: owner,
	})
	t.Cleanup(runtime.Shutdown)
	workspaceManager := workspace.NewManager(database, t.TempDir())
	handler := New(Deps{
		DB: database, Workspaces: workspaceManager,
		Runtime: runtime, EnrichmentDisabled: true,
	})

	session, err := handler.LaunchWorkspaceRuntimeService(
		ctx, workspaceID, string(localruntime.LaunchTargetPlainShell),
	)

	require.NoError(err)
	assert.NotEmpty(session.Key)
	assert.Equal(workspaceID, session.WorkspaceID)
	assert.Equal(string(localruntime.LaunchTargetPlainShell), session.TargetKey)
	stored, err := workspaceManager.RuntimeSessionsForWorkspace(ctx, workspaceID)
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal(session.Key, stored[0].SessionKey)
}
