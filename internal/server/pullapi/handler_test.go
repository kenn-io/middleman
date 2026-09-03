package pullapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

func TestHandlerRegistersPullRoutes(t *testing.T) {
	t.Parallel()

	api := humago.New(http.NewServeMux(), huma.DefaultConfig("test", "0"))
	New(Deps{}).Register(api)
	assert := assert.New(t)

	type routeContract struct {
		method string
		path   string
		status int
	}
	pull := "/pulls/{provider}/{owner}/{name}/{number}"
	hostPull := "/host/{platform_host}" + pull
	want := map[string]routeContract{
		"list-pulls":  {http.MethodGet, "/pulls", http.StatusOK},
		"list-stacks": {http.MethodGet, "/stacks", http.StatusOK},
	}
	addPair := func(id, method, suffix string, status int) {
		want[id] = routeContract{method, pull + suffix, status}
		want[id+"-on-host"] = routeContract{method, hostPull + suffix, status}
	}
	addPair("get-pull", http.MethodGet, "", http.StatusOK)
	addPair("get-pull-import-metadata", http.MethodGet, "/import-metadata", http.StatusOK)
	addPair("get-pull-commits", http.MethodGet, "/commits", http.StatusOK)
	addPair("get-pull-diff", http.MethodGet, "/diff", http.StatusOK)
	addPair("get-pull-files", http.MethodGet, "/files", http.StatusOK)
	addPair("get-pull-file-preview", http.MethodGet, "/file-preview", http.StatusOK)
	addPair("get-pull-stack", http.MethodGet, "/stack", http.StatusOK)
	addPair("set-kanban-state", http.MethodPut, "/state", http.StatusOK)
	addPair("edit-pr-content", http.MethodPatch, "", http.StatusOK)
	addPair("post-pr-comment", http.MethodPost, "/comments", http.StatusCreated)
	addPair("edit-pr-comment", http.MethodPatch, "/comments/{comment_id}", http.StatusOK)
	addPair("delete-pr-comment", http.MethodDelete, "/comments/{comment_id}", http.StatusNoContent)
	addPair("reply-to-discussion", http.MethodPost, "/discussions/{discussion_id}/reply", http.StatusCreated)
	addPair("resolve-discussion", http.MethodPost, "/discussions/{discussion_id}/resolve", http.StatusOK)
	addPair("set-pr-labels", http.MethodPut, "/labels", http.StatusOK)
	addPair("set-pr-assignees", http.MethodPut, "/assignees", http.StatusOK)
	addPair("set-pr-reviewers", http.MethodPut, "/reviewers", http.StatusOK)
	addPair("approve-pull", http.MethodPost, "/approve", http.StatusOK)
	addPair("request-pull-changes", http.MethodPost, "/request-changes", http.StatusOK)
	addPair("approve-pull-workflows", http.MethodPost, "/approve-workflows", http.StatusOK)
	addPair("mark-pull-ready-for-review", http.MethodPost, "/ready-for-review", http.StatusOK)
	addPair("merge-pull", http.MethodPost, "/merge", http.StatusOK)
	addPair("defer-merge-pull", http.MethodPost, "/merge/deferred", http.StatusAccepted)
	addPair("set-pr-github-state", http.MethodPost, "/github-state", http.StatusOK)
	addPair("get-pr-review-draft", http.MethodGet, "/review-draft", http.StatusOK)
	addPair("create-pr-review-draft-comment", http.MethodPost, "/review-draft/comments", http.StatusCreated)
	addPair("edit-pr-review-draft-comment", http.MethodPatch, "/review-draft/comments/{draft_comment_id}", http.StatusOK)
	addPair("delete-pr-review-draft-comment", http.MethodDelete, "/review-draft/comments/{draft_comment_id}", http.StatusOK)
	addPair("publish-pr-review-draft", http.MethodPost, "/review-draft/publish", http.StatusOK)
	addPair("discard-pr-review-draft", http.MethodDelete, "/review-draft", http.StatusOK)
	addPair("apply-pr-review-suggestions", http.MethodPost, "/review-suggestions/apply", http.StatusOK)
	addPair("resolve-pr-review-thread", http.MethodPost, "/review-threads/{thread_id}/resolve", http.StatusOK)
	addPair("unresolve-pr-review-thread", http.MethodPost, "/review-threads/{thread_id}/unresolve", http.StatusOK)

	gotByID := make(map[string]*huma.Operation)
	gotPathByID := make(map[string]string)
	for path, item := range api.OpenAPI().Paths {
		for _, operation := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete, item.Patch,
		} {
			if operation != nil {
				gotByID[operation.OperationID] = operation
				gotPathByID[operation.OperationID] = path
			}
		}
	}
	assert.Len(gotByID, len(want))
	for operationID, expected := range want {
		gotOperation := gotByID[operationID]
		if assert.NotNil(gotOperation, operationID) {
			assert.Equal(expected.method, gotOperation.Method, operationID)
			assert.Equal(expected.status, gotOperation.DefaultStatus, operationID)
		}
		assert.Equal(expected.path, gotPathByID[operationID], operationID)
	}
	for _, operationID := range []string{
		"sync-pull", "sync-pull-on-host",
		"refresh-pull-ci", "refresh-pull-ci-on-host",
		"enqueue-pr-sync", "enqueue-pr-sync-on-host",
	} {
		assert.NotContains(gotByID, operationID)
	}
}

func TestListPullsTreatsAssociatedWorkspaceSubjectAsHasWorkspace(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 42, Number: 42, Title: "Associated work",
		Author: "alice", State: db.MergeRequestStateOpen,
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)
	key := db.WorkspaceSubjectKey{RepoID: repoID, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42}
	handler := New(Deps{
		DB:       database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{
				OwnReferences: map[db.WorkspaceSubjectKey]workspaceapi.WorkspaceRef{},
				Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{
					key: {
						Subject: db.WorkspaceSubjectMetadata{
							Key: key, Platform: "github", PlatformHost: "github.com",
							PlatformRepoID: identity.PlatformRepoID,
						},
						Workspace: workspaceapi.WorkspaceRef{ID: "ws-adhoc", Status: "ready"},
					},
				},
			}, nil
		},
	})

	result, err := handler.listPulls(t.Context(), &listPullsInput{State: "open"})
	require.NoError(err)
	require.Len(result.Body, 1)
	require.NotNil(result.Body[0].Workspace)
	assert.Equal(t, "ws-adhoc", result.Body[0].Workspace.ID)
}

func TestListPullsReportsStackPlacementForMultiMemberStacks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	database := dbtest.Open(t)
	ctx := t.Context()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(ctx, identity)
	require.NoError(err)
	insert := func(number int, head, base string) int64 {
		id, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number, Title: "PR",
			Author: "alice", State: db.MergeRequestStateOpen, HeadBranch: head, BaseBranch: base,
			CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		})
		require.NoError(err)
		return id
	}
	firstID := insert(1, "feature/a", "main")
	secondID := insert(2, "feature/b", "feature/a")
	thirdID := insert(3, "feature/c", "feature/b")
	insert(4, "solo", "main")
	loneID := insert(5, "lone", "main")

	stackID, err := database.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	require.NoError(database.ReplaceStackMembers(ctx, stackID, []db.StackMember{
		{StackID: stackID, MergeRequestID: firstID, Position: 1},
		{StackID: stackID, MergeRequestID: secondID, Position: 2},
		{StackID: stackID, MergeRequestID: thirdID, Position: 3},
	}))
	loneStackID, err := database.UpsertStack(ctx, repoID, 5, "lone")
	require.NoError(err)
	require.NoError(database.ReplaceStackMembers(ctx, loneStackID, []db.StackMember{
		{StackID: loneStackID, MergeRequestID: loneID, Position: 1},
	}))

	handler := New(Deps{
		DB:       database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
	})
	result, err := handler.listPulls(ctx, &listPullsInput{State: "open"})
	require.NoError(err)

	byNumber := map[int]MergeRequestResponse{}
	for _, row := range result.Body {
		byNumber[row.Number] = row
	}
	require.Len(byNumber, 5)
	require.NotNil(byNumber[2].Stack)
	assert.Equal(StackPlacementResponse{Position: 2, Size: 3}, *byNumber[2].Stack)
	require.NotNil(byNumber[3].Stack)
	assert.Equal(StackPlacementResponse{Position: 3, Size: 3}, *byNumber[3].Stack)
	assert.Nil(byNumber[4].Stack, "pull requests outside a stack carry no placement")
	assert.Nil(byNumber[5].Stack, "single-member stacks are not shown as stacked")
}

type stubPullProviderSource struct {
	rows   []MergeRequestResponse
	detail MergeRequestDetailResponse
}

func (s stubPullProviderSource) ListPulls(
	context.Context, ListQuery,
) ([]MergeRequestResponse, error) {
	return s.rows, nil
}

func (s stubPullProviderSource) GetPull(
	context.Context, ItemIdentity,
) (MergeRequestDetailResponse, error) {
	return s.detail, nil
}

func (stubPullProviderSource) GetDiffDescriptor(
	context.Context, ItemIdentity,
) (providerplane.DiffDescriptor, error) {
	return providerplane.DiffDescriptor{}, errors.New("unexpected diff descriptor request")
}

func TestProviderFetchPreservesEmptyPullList(t *testing.T) {
	t.Parallel()
	handler := New(Deps{
		ProviderSource: stubPullProviderSource{rows: []MergeRequestResponse{}},
	})

	rows, err := handler.ListService(t.Context(), ListQuery{})
	require.NoError(t, err)
	require.NotNil(t, rows)
	require.Empty(t, rows)
}

func TestProviderFetchOverlaysLocalPullWorkspaceWithoutReordering(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()

	key := db.WorkspaceSubjectKey{
		RepoID: 7, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1,
	}
	handler := New(Deps{
		ProviderSource: stubPullProviderSource{rows: []MergeRequestResponse{
			{RepoID: 91, Number: 2, Repo: httpapi.RepoRefResponse{
				Provider: "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
			}},
			{RepoID: 91, Number: 1, Repo: httpapi.RepoRefResponse{
				Provider: "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
			}},
		}},
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{
				key: {
					Subject: db.WorkspaceSubjectMetadata{
						Key: key, Platform: "github", PlatformHost: "github.com",
						PlatformRepoID: "repo-widget",
					},
					Workspace: workspaceapi.WorkspaceRef{ID: "ws-local", Status: "ready"},
				},
			}}, nil
		},
	})

	rows, err := handler.ListService(t.Context(), ListQuery{})
	require.NoError(err)
	require.Len(rows, 2)
	assert.Equal([]int{2, 1}, []int{rows[0].Number, rows[1].Number})
	assert.Nil(rows[0].Workspace)
	require.NotNil(rows[1].Workspace)
	assert.Equal("ws-local", rows[1].Workspace.ID)
}

func TestProviderFetchReplacesHubPullDetailWorkspaceWithLocalWorkspace(t *testing.T) {
	t.Parallel()

	key := db.WorkspaceSubjectKey{
		RepoID: 7, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
	}
	handler := New(Deps{
		ProviderSource: stubPullProviderSource{detail: MergeRequestDetailResponse{
			MergeRequest: &db.MergeRequest{RepoID: 91, Number: 42},
			Repo: httpapi.RepoRefResponse{
				Provider: "github", PlatformHost: "github.com", PlatformRepoID: "repo-widget",
			},
			Workspace: &workspaceapi.WorkspaceRef{ID: "ws-hub", Status: "ready"},
		}},
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{
				key: {
					Subject: db.WorkspaceSubjectMetadata{
						Key: key, Platform: "github", PlatformHost: "github.com",
						PlatformRepoID: "repo-widget",
					},
					Workspace: workspaceapi.WorkspaceRef{ID: "ws-local", Status: "ready"},
				},
			}}, nil
		},
	})

	detail, err := handler.GetService(t.Context(), ItemIdentity{
		Provider: "github", PlatformHost: "github.com",
		Owner: "acme", Name: "widget", Number: 42,
	})

	require.NoError(t, err)
	require.NotNil(t, detail.Workspace)
	assert.Equal(t, "ws-local", detail.Workspace.ID)
}

func TestLocalPullOverlayNeverJoinsByNumericRepoID(t *testing.T) {
	t.Parallel()

	key := db.WorkspaceSubjectKey{
		RepoID: 1, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 42,
	}
	handler := New(Deps{
		ProviderSource: stubPullProviderSource{rows: []MergeRequestResponse{{
			RepoID: 1,
			Number: 42,
			Repo: httpapi.RepoRefResponse{
				Provider: "github", PlatformHost: "github.com", PlatformRepoID: "repo-hub",
			},
		}}},
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{
				key: {
					Subject: db.WorkspaceSubjectMetadata{
						Key: key, Platform: "github", PlatformHost: "github.com",
						PlatformRepoID: "repo-spoke",
					},
					Workspace: workspaceapi.WorkspaceRef{ID: "ws-wrong-repo", Status: "ready"},
				},
			}}, nil
		},
	})

	rows, err := handler.ListService(t.Context(), ListQuery{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Workspace)
}

func TestListPullsWorkspaceActivityRecencyIsOptIn(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	base := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(err)
	for number, activityAt := range map[int]time.Time{1: base, 2: base.Add(time.Hour)} {
		_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
			RepoID: repoID, PlatformID: int64(number), Number: number, Title: "Work",
			Author: "alice", State: db.MergeRequestStateOpen,
			CreatedAt: activityAt, UpdatedAt: activityAt, LastActivityAt: activityAt,
		})
		require.NoError(err)
	}
	key := db.WorkspaceSubjectKey{RepoID: repoID, ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 1}
	workspaceAt := base.Add(2 * time.Hour)
	handler := New(Deps{
		DB: database, Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{
				Subjects: map[db.WorkspaceSubjectKey]workspaceapi.SubjectActivity{
					key: {
						Subject: db.WorkspaceSubjectMetadata{
							Key: key, Platform: "github", PlatformHost: "github.com",
							PlatformRepoID: identity.PlatformRepoID,
						},
						Workspace:  workspaceapi.WorkspaceRef{ID: "ws-1", Status: "ready"},
						ActivityAt: &workspaceAt,
					},
				},
			}, nil
		},
	})

	disabled, err := handler.listPulls(t.Context(), &listPullsInput{State: "open"})
	require.NoError(err)
	require.Len(disabled.Body, 2)
	assert.Equal(t, 2, disabled.Body[0].Number)
	assert.Equal(t, workspaceAt.Format(time.RFC3339), disabled.Body[1].LastWorkspaceActivityAt)
	require.NotNil(disabled.Body[1].Workspace)

	handler.ApplyConfig(ConfigSnapshot{UseWorkspaceActivityForRecency: true})
	enabled, err := handler.listPulls(t.Context(), &listPullsInput{State: "open"})
	require.NoError(err)
	require.Len(enabled.Body, 2)
	assert.Equal(t, 1, enabled.Body[0].Number)
}

func TestPullDetailTreatsWorkspaceSnapshotFailureAsBestEffort(t *testing.T) {
	require := require.New(t)
	database := dbtest.Open(t)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	identity := db.GitHubRepoIdentity("github.com", "acme", "widget")
	identity.PlatformRepoID = "repo-acme-widget"
	repoID, err := database.UpsertRepo(t.Context(), identity)
	require.NoError(err)
	_, err = database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 42, Number: 42, Title: "Available pull request",
		Author: "alice", State: db.MergeRequestStateOpen,
		CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	})
	require.NoError(err)
	mr, err := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 42)
	require.NoError(err)
	require.NotNil(mr)
	handler := New(Deps{
		DB:       database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
		Syncer:   &ghclient.Syncer{},
		WorkspaceSubjects: func(context.Context) (workspaceapi.WorkspaceSubjectSnapshot, error) {
			return workspaceapi.WorkspaceSubjectSnapshot{}, errors.New("snapshot unavailable")
		},
	})

	response, err := handler.BuildDetail(t.Context(), mr)
	require.NoError(err)
	require.NotNil(response.MergeRequest)
	assert.Equal(t, 42, response.MergeRequest.Number)
	assert.Nil(t, response.Workspace)
}

func TestHandlerStopClosesAdmissionAndShutdownWaitsForWorkers(t *testing.T) {
	require := require.New(t)
	handler := New(Deps{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	require.True(handler.runBackground(func(ctx context.Context) {
		<-ctx.Done()
		close(canceled)
		<-release
	}))

	handler.Stop()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		require.FailNow("Stop did not cancel active Pull worker")
	}
	require.False(handler.runBackground(func(context.Context) {}), "Stop must close admission")

	shortCtx, shortCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer shortCancel()
	require.ErrorIs(handler.Shutdown(shortCtx), context.DeadlineExceeded)

	close(release)
	longCtx, longCancel := context.WithTimeout(t.Context(), time.Second)
	defer longCancel()
	require.NoError(handler.Shutdown(longCtx))
}

func TestApplyConfigCanRaceWithMidStackReads(t *testing.T) {
	handler := New(Deps{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 100 {
				handler.ApplyConfig(ConfigSnapshot{AllowMidStackMerges: i%2 == 0})
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				_ = handler.allowMidStackMerges()
			}
		}()
	}
	wg.Wait()
	handler.ApplyConfig(ConfigSnapshot{AllowMidStackMerges: true})
	assert.True(t, handler.allowMidStackMerges())
}
