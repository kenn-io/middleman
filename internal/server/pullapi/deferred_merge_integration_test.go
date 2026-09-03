package pullapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
)

type deferredMergeProviderBase struct {
	mu            sync.Mutex
	ref           platform.RepoRef
	mergeRequests []platform.MergeRequest
	ciChecks      map[string][]platform.CICheck
	ciErr         error
}

func (p *deferredMergeProviderBase) Platform() platform.Kind {
	return p.ref.Platform
}

func (p *deferredMergeProviderBase) Host() string {
	return p.ref.Host
}

func (p *deferredMergeProviderBase) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories:  true,
		ReadMergeRequests: true,
		ReadCI:            true,
	}
}

func (p *deferredMergeProviderBase) GetRepository(
	context.Context,
	platform.RepoRef,
) (platform.Repository, error) {
	return platform.Repository{
		Ref:                p.ref,
		PlatformID:         p.ref.PlatformID,
		PlatformExternalID: p.ref.PlatformExternalID,
		DefaultBranch:      p.ref.DefaultBranch,
	}, nil
}

func (p *deferredMergeProviderBase) ListRepositories(
	ctx context.Context,
	_ string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repo, err := p.GetRepository(ctx, p.ref)
	if err != nil {
		return nil, err
	}
	return []platform.Repository{repo}, nil
}

func (p *deferredMergeProviderBase) ListOpenMergeRequests(
	context.Context,
	platform.RepoRef,
) ([]platform.MergeRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.mergeRequests), nil
}

func (p *deferredMergeProviderBase) GetMergeRequest(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, mr := range p.mergeRequests {
		if mr.Number == number {
			return mr, nil
		}
	}
	return platform.MergeRequest{}, fmt.Errorf("missing merge request %d", number)
}

func (p *deferredMergeProviderBase) ListMergeRequestEvents(
	context.Context,
	platform.RepoRef,
	int,
) ([]platform.MergeRequestEvent, error) {
	return nil, nil
}

func (p *deferredMergeProviderBase) ListCIChecks(
	_ context.Context,
	_ platform.RepoRef,
	sha string,
) ([]platform.CICheck, error) {
	if p.ciErr != nil {
		return nil, p.ciErr
	}
	return slices.Clone(p.ciChecks[sha]), nil
}

type deferredMergeTestProvider struct {
	deferredMergeProviderBase
	mergeCh       chan deferredMergeTestMergeCall
	mergeErr      error
	mergeResults  []platform.MergeResult
	ciStarted     chan struct{}
	ciStartedOnce sync.Once
	ciRelease     chan struct{}
}

type deferredMergeTestMergeCall struct {
	Number          int
	CommitTitle     string
	CommitMessage   string
	Method          string
	ExpectedHeadSHA string
}

func (p *deferredMergeTestProvider) Capabilities() platform.Capabilities {
	caps := p.deferredMergeProviderBase.Capabilities()
	caps.MergeMutation = true
	return caps
}

func (p *deferredMergeTestProvider) ListCIChecks(
	ctx context.Context,
	ref platform.RepoRef,
	sha string,
) ([]platform.CICheck, error) {
	if p.ciStarted != nil {
		p.ciStartedOnce.Do(func() { close(p.ciStarted) })
	}
	if p.ciRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ciRelease:
		}
	}
	return p.deferredMergeProviderBase.ListCIChecks(ctx, ref, sha)
}

func (p *deferredMergeTestProvider) MergeMergeRequest(
	_ context.Context,
	_ platform.RepoRef,
	number int,
	commitTitle string,
	commitMessage string,
	method string,
	expectedHeadSHA string,
) (platform.MergeResult, error) {
	if p.mergeCh != nil {
		p.mergeCh <- deferredMergeTestMergeCall{
			Number:          number,
			CommitTitle:     commitTitle,
			CommitMessage:   commitMessage,
			Method:          method,
			ExpectedHeadSHA: expectedHeadSHA,
		}
	}
	if p.mergeErr != nil {
		return platform.MergeResult{}, p.mergeErr
	}
	result := platform.MergeResult{Merged: true, SHA: "merge-sha", Message: "merged"}
	// Reflect the merge the way the real provider does: the handler's
	// canonical post-merge resync reads the MR back to record the terminal
	// transition, and an unchanged state or stale updated_at would leave
	// the local row open.
	p.mu.Lock()
	if len(p.mergeResults) > 0 {
		result = p.mergeResults[0]
		p.mergeResults = p.mergeResults[1:]
	}
	if result.Merged {
		for i := range p.mergeRequests {
			if p.mergeRequests[i].Number != number ||
				p.mergeRequests[i].State != "open" {
				continue
			}
			mergedAt := p.mergeRequests[i].UpdatedAt.Add(time.Minute)
			p.mergeRequests[i].State = "merged"
			p.mergeRequests[i].MergedAt = &mergedAt
			p.mergeRequests[i].MergedBy = "ada"
			p.mergeRequests[i].UpdatedAt = mergedAt
			p.mergeRequests[i].LastActivityAt = mergedAt
		}
	}
	p.mu.Unlock()
	return result, nil
}

type deferredMergeTestOptions struct {
	deferredMergeMaxWait   time.Duration
	queueWorkspaceDeletion func(context.Context, string, string) error
}

type deferredMergeTestRecordedEvent struct {
	Event Event
}

type deferredMergeTestHub struct {
	events <-chan deferredMergeTestRecordedEvent
}

func (h *deferredMergeTestHub) Subscribe(
	context.Context,
	bool,
) (<-chan deferredMergeTestRecordedEvent, uint64) {
	return h.events, 0
}

type deferredMergeRouteServer struct {
	handler *Handler
	hub     deferredMergeTestHub
}

func (s *deferredMergeRouteServer) Hub() *deferredMergeTestHub {
	return &s.hub
}

func newDeferredMergeHTTPFixture(
	t *testing.T,
	database *db.DB,
	syncer *ghclient.Syncer,
	now time.Time,
	maxWait time.Duration,
	options ...deferredMergeTestOptions,
) (*deferredMergeRouteServer, *apiclient.Client) {
	t.Helper()
	opts := deferredMergeTestOptions{deferredMergeMaxWait: maxWait}
	if len(options) > 0 {
		opts = options[0]
		if opts.deferredMergeMaxWait <= 0 {
			opts.deferredMergeMaxWait = maxWait
		}
	}

	events := make(chan deferredMergeTestRecordedEvent, 64)
	resolver := httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{
		DB: database,
		ProviderCapabilities: func(kind platform.Kind, host string) (platform.Capabilities, error) {
			return syncer.ProviderCapabilities(kind, host)
		},
	})
	handler := New(Deps{
		DB:                     database,
		Resolver:               resolver,
		Syncer:                 syncer,
		QueueWorkspaceDeletion: opts.queueWorkspaceDeletion,
		Now:                    func() time.Time { return now },
		DeferredMergeMaxWait:   opts.deferredMergeMaxWait,
		Broadcast: func(event Event) uint64 {
			events <- deferredMergeTestRecordedEvent{Event: event}
			return 0
		},
	})

	mux := http.NewServeMux()
	api := humago.NewWithPrefix(mux, "/api/v1", huma.DefaultConfig("test", "0"))
	handler.Register(api)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, handler.Shutdown(shutdownCtx))
	})
	client, err := apiclient.New(httpServer.URL)
	require.NoError(t, err)
	return &deferredMergeRouteServer{
		handler: handler,
		hub:     deferredMergeTestHub{events: events},
	}, client
}

func newDeferredMergeRouteServer(
	t *testing.T,
	provider *deferredMergeTestProvider,
	ref platform.RepoRef,
	now time.Time,
	initialChecks []db.CICheck,
	options ...deferredMergeTestOptions,
) (*deferredMergeRouteServer, *db.DB, int64, *apiclient.Client) {
	t.Helper()
	ctx := t.Context()
	registry, err := platform.NewRegistry(provider)
	require.NoError(t, err)
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(ctx, platform.DBRepoIdentity(ref))
	require.NoError(t, err)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON:    mustDeferredMergeChecksJSON(t, initialChecks),
		CIHadPending:    true,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
	})
	require.NoError(t, err)

	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform:           platform.KindGitLab,
			PlatformHost:       ref.Host,
			Owner:              ref.Owner,
			Name:               ref.Name,
			RepoPath:           ref.RepoPath,
			PlatformRepoID:     ref.PlatformID,
			PlatformExternalID: ref.PlatformExternalID,
		}},
		time.Minute,
		nil,
		map[string]*ghclient.SyncBudget{
			ghclient.RateBucketKey("gitlab", ref.Host, "host"): ghclient.NewSyncBudget(100),
		},
	)
	t.Cleanup(syncer.Stop)
	opts := deferredMergeTestOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	srv, client := newDeferredMergeHTTPFixture(
		t,
		database,
		syncer,
		now,
		opts.deferredMergeMaxWait,
		opts,
	)
	return srv, database, repoID, client
}

func TestDeferMergeEndpointQueuesMergeAndBroadcastsCompletion(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Defer merge",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CIStatus:       "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	database := dbtest.Open(t)
	repoID, err := database.UpsertRepo(ctx, platform.DBRepoIdentity(ref))
	require.NoError(err)
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database,
		nil,
		[]ghclient.RepoRef{{
			Platform:     platform.KindGitLab,
			PlatformHost: ref.Host,
			Owner:        ref.Owner,
			Name:         ref.Name,
			RepoPath:     ref.RepoPath,
		}},
		time.Minute,
		nil,
		map[string]*ghclient.SyncBudget{
			ghclient.RateBucketKey("gitlab", ref.Host, "host"): ghclient.NewSyncBudget(100),
		},
	)
	t.Cleanup(syncer.Stop)
	var deletedID string
	srv, client := newDeferredMergeHTTPFixture(
		t,
		database,
		syncer,
		now,
		0,
		deferredMergeTestOptions{
			queueWorkspaceDeletion: func(_ context.Context, _ string, id string) error {
				deletedID = id
				return nil
			},
		},
	)
	events, _ := srv.Hub().Subscribe(ctx, false)
	expectedHeadSHA := "head-sha"
	workspaceID := "ws-1"

	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{
			CommitTitle:       "Merge title",
			CommitMessage:     "Merge body",
			Method:            "squash",
			ExpectedHeadSha:   &expectedHeadSHA,
			DeleteWorkspaceId: &workspaceID,
		},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.Equal("queued", resp.JSON202.Status)
	require.Equal(int64(1), resp.JSON202.PendingChecks)

	var mergeCall deferredMergeTestMergeCall
	select {
	case mergeCall = <-provider.mergeCh:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred merge")
	}
	require.Equal(7, mergeCall.Number)
	require.Equal("squash", mergeCall.Method)
	require.Equal("head-sha", mergeCall.ExpectedHeadSHA)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge completion event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("merged", completed.Status)
	require.True(completed.Merged)
	require.Equal("merge-sha", completed.SHA)
	require.Equal("2026-06-15T12:00:00Z", completed.CompletedAt)
	require.Empty(completed.Warning)
	require.True(completed.WorkspaceCleanupPending)
	require.Equal("ws-1", deletedID)

	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	require.Equal("merged", string(stored.State))

	// Clients refresh detail the moment they receive
	// deferred_merge_completed, so pending must already be false on the
	// very first read after the event — not eventually.
	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.False(detailResp.JSON200.DeferredMergePending)
}

func TestPullDetailReportsDeferredMergePendingWhileQueued(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:    "GitLab",
				Name:   "pipeline",
				Status: "in_progress",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, _, _, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "in_progress"}},
	)

	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.False(detailResp.JSON200.DeferredMergePending)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	// The pending check never completes, so the background worker keeps
	// waiting; the detail response must report the queued merge.
	detailResp, err = client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.True(detailResp.JSON200.DeferredMergePending)
}

func TestImmediateMergeSupersedesQueuedDeferredMerge(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		// The provider's own view of the MR: the immediate merge flips
		// it to merged, and the handler's canonical post-merge resync
		// reads it back to record the terminal transition.
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Defer merge",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CIStatus:       "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:    "GitLab",
				Name:   "pipeline",
				Status: "in_progress",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 2),
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "in_progress"}},
	)
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	deferResp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, deferResp.StatusCode(), string(deferResp.Body))

	mergeResp, err := client.HTTP.MergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(200, mergeResp.StatusCode(), string(mergeResp.Body))

	// The immediate merge supersedes the queued worker: pending clears with
	// the merge response, not on the worker's next poll.
	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.False(detailResp.JSON200.DeferredMergePending)

	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)
	require.Equal("merged", string(stored.State))

	// The superseded worker must stand down silently: a deferred-merge
	// failure event for a pull request the user just merged is misleading.
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			require.NotEqual("deferred_merge_completed", ev.Event.Type)
		case <-deadline:
			return
		}
	}
}

func TestImmediateUnmergedResponsePreservesQueueUntilDeferredProviderRejects(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Defer merge",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CIStatus:       "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 2),
		mergeResults: []platform.MergeResult{
			{
				Merged:  false,
				Message: "immediate provider response did not merge the pull request",
			},
			{
				Merged:  false,
				Message: "deferred provider response did not merge the pull request",
			},
		},
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, _, _, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "in_progress"}},
	)
	events, _ := srv.Hub().Subscribe(ctx, false)
	expectedHeadSHA := "head-sha"

	deferResp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, deferResp.StatusCode(), string(deferResp.Body))
	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh")
	}

	mergeResp, err := client.HTTP.MergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(200, mergeResp.StatusCode(), string(mergeResp.Body))
	require.NotNil(mergeResp.JSON200)
	require.False(mergeResp.JSON200.Merged)

	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.True(detailResp.JSON200.DeferredMergePending)

	close(ciRelease)
	var completed DeferredMergeCompletedPayload
	for completed.Status == "" {
		select {
		case event := <-events:
			if event.Event.Type != "deferred_merge_completed" {
				continue
			}
			var ok bool
			completed, ok = event.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge completion")
		}
	}
	require.Equal("failed", completed.Status)
	require.False(completed.Merged)
	require.Equal("deferred provider response did not merge the pull request", completed.Error)

	detailResp, err = client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.False(detailResp.JSON200.DeferredMergePending)
}

func TestDeferMergeEndpointRejectsInvalidMergeMethodBeforeQueueing(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, _, _, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})

	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "fast-forward"},
	)
	require.NoError(err)
	require.Equal(400, resp.StatusCode(), string(resp.Body))
	require.Contains(string(resp.Body), "invalid merge method")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointRejectsWithoutPendingChecks(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success",
	}})
	require.NoError(database.UpdateMRCIStatus(
		ctx,
		repoID,
		7,
		"success",
		mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success",
		}}),
	))

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(409, resp.StatusCode(), string(resp.Body))
	require.Contains(string(resp.Body), "no_pending_checks")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointRejectsMissingBaseSHA(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	_, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(409, resp.StatusCode(), string(resp.Body))
	require.Contains(string(resp.Body), "base_unknown")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointRejectsFailedAggregateCIWithPassingRows(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success",
	}})
	require.NoError(database.UpdateMRCIStatus(
		ctx,
		repoID,
		7,
		"failure",
		mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success",
		}}),
	))

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(409, resp.StatusCode(), string(resp.Body))
	require.Contains(string(resp.Body), "ci_failed")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointFailsWhenAggregatePendingRefreshBecomesUnknown(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:      ref,
		ciChecks: map[string][]platform.CICheck{"head-sha": {}},
		mergeCh:  make(chan deferredMergeTestMergeCall, 1),
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success"}},
		deferredMergeTestOptions{deferredMergeMaxWait: 10 * time.Millisecond},
	)
	require.NoError(database.UpdateMRCIStatus(
		ctx,
		repoID,
		7,
		"pending",
		mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success",
		}}),
	))
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.Equal(int64(0), resp.JSON202.PendingChecks)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for aggregate-unknown deferred merge failure")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "aggregate CI status is unavailable")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointFailsWhenGranularPendingRefreshHasUnknownAggregate(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:      ref,
		ciChecks: map[string][]platform.CICheck{"head-sha": {}},
		mergeCh:  make(chan deferredMergeTestMergeCall, 1),
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "in_progress"}},
	)
	require.NoError(database.UpdateMRCIStatus(
		ctx,
		repoID,
		7,
		"",
		mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
	))
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.Equal(int64(1), resp.JSON202.PendingChecks)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for aggregate-unknown deferred merge failure")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "aggregate CI status is unavailable")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointRefreshesEmptyPendingSnapshotBeforeRejecting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:    "GitLab",
				Name:   "pipeline",
				Status: "in_progress",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, nil, deferredMergeTestOptions{
		deferredMergeMaxWait: 10 * time.Millisecond,
	})
	require.NoError(database.UpdateMRCIStatus(ctx, repoID, 7, "", "[]"))

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))
	require.NotNil(resp.JSON202)
	require.Equal(int64(1), resp.JSON202.PendingChecks)
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenCIRefreshWarns(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		ciErr:   errors.New("gitlab pipeline API unavailable"),
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	srv, _, _, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge failure event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "could not refresh CI checks")
	require.Equal("2026-06-15T12:00:00Z", completed.CompletedAt)
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenCurrentChecksFail(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {
				{App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success"},
				{App: "GitLab", Name: "security", Status: "completed", Conclusion: "failure"},
			},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	srv, _, _, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge failure event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "check failed")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenHeadChangesWhileWaiting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh:   make(chan deferredMergeTestMergeCall, 1),
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh to start")
	}
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "open",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "new-head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	close(ciRelease)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge stale-head event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "target changed")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenProviderBaseChangesBeforeMerge(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Defer merge",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CIStatus:       "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh:   make(chan deferredMergeTestMergeCall, 1),
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, _, _, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh to start")
	}
	provider.mergeRequests[0].BaseSHA = "new-base-sha"
	close(ciRelease)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge stale-base event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "target changed")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenPendingChecksTimeOut(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:    "GitLab",
				Name:   "pipeline",
				Status: "in_progress",
			}},
		},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	srv, _, _, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "in_progress"}},
		deferredMergeTestOptions{deferredMergeMaxWait: 10 * time.Millisecond},
	)
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge timeout event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "timed out waiting for pending CI checks")
	// Failure events follow the same ordering contract as completions:
	// pending is cleared before the broadcast, so the first detail read
	// after the event must not report a queued merge.
	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(200, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.False(detailResp.JSON200.DeferredMergePending)
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointRejectsClosedPullRequest(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	provider := &deferredMergeTestProvider{
		ref:     ref,
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	// Closing a pull request is the only cancel a user has for a queued
	// deferred merge, so queueing one on a closed pull request must be
	// rejected outright rather than spawning a worker that waits on CI.
	_, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "closed",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(409, resp.StatusCode(), string(resp.Body))
	require.Contains(string(resp.Body), "not_open")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenTargetClosedWhileWaiting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh:   make(chan deferredMergeTestMergeCall, 1),
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh to start")
	}
	// The pull request is closed mid-wait, even though its head and base are
	// unchanged. The worker must abort instead of merging a retracted target.
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "closed",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	close(ciRelease)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge closed-target event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "no longer open")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func TestDeferMergeEndpointStandsDownSilentlyWhenTargetMergedWhileWaiting(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh:   make(chan deferredMergeTestMergeCall, 1),
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, database, repoID, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh to start")
	}
	// The pull request is merged through another path (an immediate merge or
	// an external merge observed via sync) while the worker waits. Unlike a
	// close, the merged outcome satisfies the queued intent, so the worker
	// must stand down silently instead of broadcasting a misleading failure.
	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:          repoID,
		PlatformID:      7001,
		Number:          7,
		URL:             "https://gitlab.example.com/group/project/-/merge_requests/7",
		Title:           "Defer merge",
		Author:          "ada",
		State:           "merged",
		HeadBranch:      "feature",
		BaseBranch:      "main",
		PlatformHeadSHA: "head-sha",
		PlatformBaseSHA: "base-sha",
		CIStatus:        "pending",
		CIChecksJSON: mustDeferredMergeChecksJSON(t, []db.CICheck{{
			App: "GitLab", Name: "pipeline", Status: "in_progress",
		}}),
		CIHadPending:   true,
		CreatedAt:      now,
		UpdatedAt:      now,
		LastActivityAt: now,
	})
	require.NoError(err)
	close(ciRelease)

	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			require.NotEqual("deferred_merge_completed", ev.Event.Type)
		case <-deadline:
			return
		}
	}
}

func TestDeferMergeEndpointBroadcastsFailureWhenProviderClosedBeforeMerge(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	ciStarted := make(chan struct{})
	ciRelease := make(chan struct{})
	provider := &deferredMergeTestProvider{
		ref: ref,
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Defer merge",
			Author:         "ada",
			State:          "open",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CIStatus:       "pending",
			CreatedAt:      now,
			UpdatedAt:      now,
			LastActivityAt: now,
		}},
		ciChecks: map[string][]platform.CICheck{
			"head-sha": {{
				App:        "GitLab",
				Name:       "pipeline",
				Status:     "completed",
				Conclusion: "success",
			}},
		},
		mergeCh:   make(chan deferredMergeTestMergeCall, 1),
		ciStarted: ciStarted,
		ciRelease: ciRelease,
	}
	srv, _, _, client := newDeferredMergeRouteServer(t, provider, ref, now, []db.CICheck{{
		App: "GitLab", Name: "pipeline", Status: "in_progress",
	}})
	events, _ := srv.Hub().Subscribe(ctx, false)

	expectedHeadSHA := "head-sha"
	resp, err := client.HTTP.DeferMergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(202, resp.StatusCode(), string(resp.Body))

	select {
	case <-ciStarted:
	case <-time.After(time.Second):
		require.FailNow("timed out waiting for deferred CI refresh to start")
	}
	// The local row still says open, but the provider reports the pull request
	// closed (e.g. closed-to-cancel before the next sync). The authoritative
	// pre-merge provider check must block the merge.
	provider.mergeRequests[0].State = "closed"
	close(ciRelease)

	var completed DeferredMergeCompletedPayload
	for range 4 {
		select {
		case ev := <-events:
			if ev.Event.Type != "deferred_merge_completed" {
				continue
			}
			payload, ok := ev.Event.Data.(DeferredMergeCompletedPayload)
			require.True(ok)
			completed = payload
		case <-time.After(time.Second):
			require.FailNow("timed out waiting for deferred merge provider-closed event")
		}
		if completed.Status != "" {
			break
		}
	}
	require.Equal("failed", completed.Status)
	require.Contains(completed.Error, "no longer open")
	select {
	case call := <-provider.mergeCh:
		require.Failf("unexpected merge", "merge call: %+v", call)
	default:
	}
}

func mustDeferredMergeChecksJSON(t *testing.T, checks []db.CICheck) string {
	t.Helper()
	raw, err := json.Marshal(checks)
	require.NoError(t, err)
	return string(raw)
}

func TestImmediateMergeRecordsMergedActor(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.example.com",
		Owner:              "group",
		Name:               "project",
		RepoPath:           "group/project",
		PlatformID:         4242,
		PlatformExternalID: "gid://gitlab/Project/4242",
		DefaultBranch:      "main",
	}
	mergedAt := now.Add(time.Minute)
	provider := &deferredMergeTestProvider{
		ref: ref,
		// The provider view after the merge lands: merged, with the
		// acting user. The handler's synchronous canonical resync
		// must persist this actor as an authored merged event before
		// the merge response returns.
		mergeRequests: []platform.MergeRequest{{
			Repo:           ref,
			PlatformID:     7001,
			Number:         7,
			URL:            "https://gitlab.example.com/group/project/-/merge_requests/7",
			Title:          "Merge me",
			Author:         "ada",
			State:          "merged",
			MergedAt:       &mergedAt,
			MergedBy:       "merge-admin",
			HeadBranch:     "feature",
			BaseBranch:     "main",
			HeadSHA:        "head-sha",
			BaseSHA:        "base-sha",
			CreatedAt:      now,
			UpdatedAt:      mergedAt,
			LastActivityAt: mergedAt,
		}},
		mergeCh: make(chan deferredMergeTestMergeCall, 1),
	}
	_, database, repoID, client := newDeferredMergeRouteServer(
		t,
		provider,
		ref,
		now,
		[]db.CICheck{{App: "GitLab", Name: "pipeline", Status: "completed", Conclusion: "success"}},
	)

	expectedHeadSHA := "head-sha"
	mergeResp, err := client.HTTP.MergePullOnHostWithResponse(
		ctx,
		ref.Host,
		"gitlab",
		ref.Owner,
		ref.Name,
		7,
		generated.MergePRInputBody{Method: "squash", ExpectedHeadSha: &expectedHeadSHA},
	)
	require.NoError(err)
	require.Equal(200, mergeResp.StatusCode(), string(mergeResp.Body))

	stored, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repoID, 7)
	require.NoError(err)
	require.NotNil(stored)

	require.Eventually(func() bool {
		events, err := database.ListMREvents(ctx, stored.ID)
		require.NoError(err)
		for _, event := range events {
			if event.EventType == "merged" && event.Author == "merge-admin" {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond,
		"no authored merged event recorded after immediate merge")

	detailResp, err := client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.NotNil(detailResp.JSON200.Events)
	mergedEvents := make([]generated.MergeRequestEventResponse, 0, 1)
	for _, event := range detailResp.JSON200.Events {
		if event.EventType == "merged" {
			mergedEvents = append(mergedEvents, event)
		}
	}
	require.Len(mergedEvents, 1,
		"detail must not combine an authored event with a synthetic merge event")
	require.Equal("merge-admin", mergedEvents[0].Author)
	require.True(mergedEvents[0].CreatedAt.Equal(mergedAt),
		"the merged event must carry the provider's merged_at, not a local clock")
	require.NotNil(detailResp.JSON200.MergeRequest.MergedAt)
	require.True(detailResp.JSON200.MergeRequest.MergedAt.Equal(mergedEvents[0].CreatedAt))

	// Simulate a later accepted provider snapshot replacing the eager local
	// merge timestamp. Existing databases may briefly contain an authored
	// event at the old time; the detail response must still show one merge.
	require.NoError(database.UpdateMRState(
		ctx, repoID, 7, "merged", &mergedAt, &mergedAt,
	))
	detailResp, err = client.HTTP.GetPullOnHostWithResponse(
		ctx, ref.Host, "gitlab", ref.Owner, ref.Name, 7,
	)
	require.NoError(err)
	require.Equal(http.StatusOK, detailResp.StatusCode(), string(detailResp.Body))
	require.NotNil(detailResp.JSON200)
	require.NotNil(detailResp.JSON200.Events)
	mergedEvents = mergedEvents[:0]
	for _, event := range detailResp.JSON200.Events {
		if event.EventType == "merged" {
			mergedEvents = append(mergedEvents, event)
		}
	}
	require.Len(mergedEvents, 1,
		"detail must not synthesize a second merge beside an authored event")
	require.Equal("merge-admin", mergedEvents[0].Author)
}
