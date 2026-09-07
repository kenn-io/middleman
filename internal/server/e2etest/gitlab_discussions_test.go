package e2etest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/testutil/servertest"
	"go.kenn.io/forge/platform"
)

func TestGetPRDetailIncludesThreadID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	srv, database := setupTestServer(t)

	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.com",
		PlatformRepoID: "gid://gitlab/Project/4242",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:          "Discussion test",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	threadID := "disc-abc123"
	platformID := int64(101)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "needs fix",
		CreatedAt:      time.Now().UTC(),
		DedupeKey:      "note-101",
		ThreadID:       &threadID,
		PositionJSON:   `{"new_path":"main.go","new_line":42}`,
		Resolvable:     true,
		Resolved:       false,
	}}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pulls/gitlab/acme/widget/7", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)

	var result struct {
		Events []struct {
			ThreadID   *string `json:"ThreadID"`
			Resolvable bool    `json:"Resolvable"`
			Resolved   bool    `json:"Resolved"`
		} `json:"events"`
	}
	err = json.NewDecoder(rr.Body).Decode(&result)
	require.NoError(err)

	require.Len(result.Events, 1)
	assert.NotNil(result.Events[0].ThreadID)
	assert.Equal("disc-abc123", *result.Events[0].ThreadID)
	assert.True(result.Events[0].Resolvable)
	assert.False(result.Events[0].Resolved)
}

func TestGitLabDiscussionMetadataSyncsToDetailAPI(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	now := time.Now().UTC()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	mrThreadID := "disc-mr-abc123"
	issueThreadID := "disc-issue-def456"
	provider := &gitLabDiscussionProvider{
		ref: ref,
		mergeRequests: map[int]platform.MergeRequest{
			7: {
				Repo:               ref,
				PlatformID:         1001,
				PlatformExternalID: "gid://gitlab/MergeRequest/1001",
				Number:             7,
				URL:                "https://gitlab.com/acme/widget/-/merge_requests/7",
				Title:              "Discussion sync test",
				Author:             "author",
				State:              "open",
				CreatedAt:          now,
				UpdatedAt:          now,
				LastActivityAt:     now,
			},
		},
		mergeRequestEvents: map[int][]platform.MergeRequestEvent{
			7: {{
				Repo:               ref,
				PlatformID:         2001,
				PlatformExternalID: "gid://gitlab/Note/2001",
				MergeRequestNumber: 7,
				EventType:          "issue_comment",
				Author:             "reviewer",
				Body:               "Please fix this line.",
				CreatedAt:          now,
				DedupeKey:          "gitlab-note-2001",
				ThreadID:           mrThreadID,
				PositionJSON:       `{"new_path":"main.go","new_line":42}`,
				Resolvable:         true,
				Resolved:           false,
			}},
		},
		issues: map[int]platform.Issue{
			11: {
				Repo:               ref,
				PlatformID:         3001,
				PlatformExternalID: "gid://gitlab/Issue/3001",
				Number:             11,
				URL:                "https://gitlab.com/acme/widget/-/issues/11",
				Title:              "Issue discussion sync test",
				Author:             "author",
				State:              "open",
				CreatedAt:          now,
				UpdatedAt:          now,
				LastActivityAt:     now,
			},
		},
		issueEvents: map[int][]platform.IssueEvent{
			11: {{
				Repo:               ref,
				PlatformID:         4001,
				PlatformExternalID: "gid://gitlab/Note/4001",
				IssueNumber:        11,
				EventType:          "issue_comment",
				Author:             "triager",
				Body:               "Issue discussion reply.",
				CreatedAt:          now,
				DedupeKey:          "gitlab-issue-note-4001",
				ThreadID:           issueThreadID,
			}},
		},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	require.NoError(syncer.SyncMROnProvider(ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", 7))
	require.NoError(syncer.SyncIssueOnProvider(ctx, platform.KindGitLab, "gitlab.com", "acme", "widget", 11))

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})

	prReq := httptest.NewRequest(http.MethodGet, "/api/v1/pulls/gitlab/acme/widget/7", nil)
	prRR := httptest.NewRecorder()
	srv.ServeHTTP(prRR, prReq)

	require.Equal(http.StatusOK, prRR.Code, "response: %s", prRR.Body.String())
	var prResult struct {
		Events []struct {
			ThreadID   *string `json:"ThreadID"`
			Resolvable bool    `json:"Resolvable"`
			Resolved   bool    `json:"Resolved"`
		} `json:"events"`
	}
	err = json.NewDecoder(prRR.Body).Decode(&prResult)
	require.NoError(err)
	require.Len(prResult.Events, 1)
	require.NotNil(prResult.Events[0].ThreadID)
	assert.Equal(mrThreadID, *prResult.Events[0].ThreadID)
	assert.True(prResult.Events[0].Resolvable)
	assert.False(prResult.Events[0].Resolved)

	issueReq := httptest.NewRequest(http.MethodGet, "/api/v1/issues/gitlab/acme/widget/11", nil)
	issueRR := httptest.NewRecorder()
	srv.ServeHTTP(issueRR, issueReq)

	require.Equal(http.StatusOK, issueRR.Code, "response: %s", issueRR.Body.String())
	var issueResult struct {
		Events []struct {
			ThreadID *string `json:"ThreadID"`
		} `json:"events"`
	}
	err = json.NewDecoder(issueRR.Body).Decode(&issueResult)
	require.NoError(err)
	require.Len(issueResult.Events, 1)
	require.NotNil(issueResult.Events[0].ThreadID)
	assert.Equal(issueThreadID, *issueResult.Events[0].ThreadID)
}

type gitLabDiscussionProvider struct {
	ref platform.RepoRef

	mergeRequests      map[int]platform.MergeRequest
	mergeRequestEvents map[int][]platform.MergeRequestEvent
	issues             map[int]platform.Issue
	issueEvents        map[int][]platform.IssueEvent

	// Track mutation calls for test assertions
	replyToDiscussionCalls []replyToDiscussionCall
	resolveDiscussionCalls []resolveDiscussionCall
}

type replyToDiscussionCall struct {
	Ref      platform.RepoRef
	Number   int
	ThreadID string
	Body     string
}

type resolveDiscussionCall struct {
	Ref      platform.RepoRef
	Number   int
	ThreadID string
	Resolved bool
}

func (p *gitLabDiscussionProvider) Platform() platform.Kind {
	return platform.KindGitLab
}

func (p *gitLabDiscussionProvider) Host() string {
	return p.ref.Host
}

func (p *gitLabDiscussionProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadRepositories:  true,
		ReadMergeRequests: true,
		ReadIssues:        true,
		ThreadReply:       true,
		ThreadResolve:     true,
	}
}

func (p *gitLabDiscussionProvider) GetRepository(_ context.Context, _ platform.RepoRef) (platform.Repository, error) {
	return platform.Repository{
		Ref:                p.ref,
		PlatformID:         p.ref.PlatformID,
		PlatformExternalID: p.ref.PlatformExternalID,
		DefaultBranch:      p.ref.DefaultBranch,
		WebURL:             p.ref.WebURL,
		CloneURL:           p.ref.CloneURL,
	}, nil
}

func (p *gitLabDiscussionProvider) ListRepositories(context.Context, string, platform.RepositoryListOptions) ([]platform.Repository, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) ListOpenMergeRequests(
	context.Context, platform.RepoRef,
) ([]platform.MergeRequest, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) GetMergeRequest(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.MergeRequest, error) {
	if mr, ok := p.mergeRequests[number]; ok {
		return mr, nil
	}
	return platform.MergeRequest{}, platform.ProviderContract(platform.KindGitLab, p.ref.Host, "merge_request", nil)
}

func (p *gitLabDiscussionProvider) ListMergeRequestEvents(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) ([]platform.MergeRequestEvent, error) {
	if p.mergeRequestEvents != nil {
		return p.mergeRequestEvents[number], nil
	}
	return nil, nil
}

func (p *gitLabDiscussionProvider) ListOpenIssues(
	context.Context, platform.RepoRef,
) ([]platform.Issue, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) GetIssue(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) (platform.Issue, error) {
	if issue, ok := p.issues[number]; ok {
		return issue, nil
	}
	return platform.Issue{}, platform.ProviderContract(platform.KindGitLab, p.ref.Host, "issue", nil)
}

func (p *gitLabDiscussionProvider) ListIssueEvents(
	_ context.Context,
	_ platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	if p.issueEvents != nil {
		return p.issueEvents[number], nil
	}
	return nil, nil
}

func (p *gitLabDiscussionProvider) ListReleases(context.Context, platform.RepoRef) ([]platform.Release, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) ListTags(context.Context, platform.RepoRef) ([]platform.Tag, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) ListCIChecks(context.Context, platform.RepoRef, string) ([]platform.CICheck, error) {
	return nil, nil
}

func (p *gitLabDiscussionProvider) ReplyToThread(
	_ context.Context,
	ref platform.RepoRef,
	number int,
	threadID string,
	body string,
) (platform.MergeRequestEvent, error) {
	p.replyToDiscussionCalls = append(p.replyToDiscussionCalls, replyToDiscussionCall{
		Ref:      ref,
		Number:   number,
		ThreadID: threadID,
		Body:     body,
	})
	return platform.MergeRequestEvent{
		Repo:               ref,
		PlatformID:         99999,
		PlatformExternalID: "99999",
		MergeRequestNumber: number,
		EventType:          "issue_comment",
		Author:             "test-user",
		Body:               body,
		CreatedAt:          time.Now().UTC(),
		DedupeKey:          "reply-" + threadID,
		ThreadID:           threadID,
	}, nil
}

func (p *gitLabDiscussionProvider) ResolveThread(
	_ context.Context,
	ref platform.RepoRef,
	number int,
	threadID string,
	resolved bool,
) error {
	p.resolveDiscussionCalls = append(p.resolveDiscussionCalls, resolveDiscussionCall{
		Ref:      ref,
		Number:   number,
		ThreadID: threadID,
		Resolved: resolved,
	})
	return nil
}

func TestGitLabRepoCapabilitiesIncludeDiscussions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repo/gitlab/acme/widget", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code)

	var result struct {
		Capabilities struct {
			ThreadReply   bool `json:"thread_reply"`
			ThreadResolve bool `json:"thread_resolve"`
		} `json:"capabilities"`
	}
	err = json.NewDecoder(rr.Body).Decode(&result)
	require.NoError(err)

	assert.True(result.Capabilities.ThreadReply)
	assert.True(result.Capabilities.ThreadResolve)
}

func TestReplyToDiscussionE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	// Create an MR to reply to
	dbRepo, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)
	require.NotNil(dbRepo)

	collidingRepoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:     "github",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)

	collidingMRID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         collidingRepoID,
		PlatformID:     9001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/pull/7",
		Title:          "Colliding PR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         dbRepo.ID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:          "Test MR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	// Valid 40-char hex thread ID
	threadID := "abc123def456789012345678901234567890abcd"
	body := `{"body":"This is my reply"}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+threadID+"/reply",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusCreated, rr.Code, "response: %s", rr.Body.String())

	// Verify the provider was called with correct arguments
	require.Len(provider.replyToDiscussionCalls, 1)
	call := provider.replyToDiscussionCalls[0]
	assert.Equal(7, call.Number)
	assert.Equal(threadID, call.ThreadID)
	assert.Equal("This is my reply", call.Body)
	assert.Equal("acme", call.Ref.Owner)
	assert.Equal("widget", call.Ref.Name)

	// Verify the reply event was persisted
	var result struct {
		Author   string  `json:"Author"`
		Body     string  `json:"Body"`
		ThreadID *string `json:"ThreadID"`
	}
	err = json.NewDecoder(rr.Body).Decode(&result)
	require.NoError(err)
	assert.Equal("test-user", result.Author)
	assert.Equal("This is my reply", result.Body)
	require.NotNil(result.ThreadID)
	assert.Equal(threadID, *result.ThreadID)

	gitlabEvents, err := database.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(gitlabEvents, 1)
	assert.Equal("This is my reply", gitlabEvents[0].Body)

	line := 12
	require.NoError(database.UpsertMRReviewThreads(ctx, mrID, []db.MRReviewThread{{
		ProviderThreadID:  threadID,
		ProviderCommentID: "note-101",
		Body:              "Root inline comment",
		AuthorLogin:       "reviewer",
		Range: db.ReviewLineRange{
			Path:     "main.go",
			Side:     "right",
			Line:     line,
			NewLine:  &line,
			LineType: "add",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}}))
	threads, err := database.ListMRReviewThreads(ctx, mrID)
	require.NoError(err)
	require.Len(threads, 1)
	localThreadID := strconv.FormatInt(threads[0].ID, 10)

	body = `{"body":"Local thread id reply"}`
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+localThreadID+"/reply",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusCreated, rr.Code, "response: %s", rr.Body.String())
	require.Len(provider.replyToDiscussionCalls, 2)
	localCall := provider.replyToDiscussionCalls[1]
	assert.Equal(threadID, localCall.ThreadID)
	assert.Equal("Local thread id reply", localCall.Body)

	err = json.NewDecoder(rr.Body).Decode(&result)
	require.NoError(err)
	assert.Equal("Local thread id reply", result.Body)
	require.NotNil(result.ThreadID)
	assert.Equal(threadID, *result.ThreadID)

	gitlabEvents, err = database.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(gitlabEvents, 1)
	assert.Equal("Local thread id reply", gitlabEvents[0].Body)

	collidingEvents, err := database.ListMREvents(ctx, collidingMRID)
	require.NoError(err)
	require.Empty(collidingEvents)
}

func TestReplyToDiscussionRejectsInvalidThreadID(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	dbRepo2, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)
	require.NotNil(dbRepo2)

	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         dbRepo2.ID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:          "Test MR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	// Test various invalid thread IDs (URL-safe but invalid for GitLab)
	invalidIDs := []string{
		"..-..-..-..-etc-passwd---------",          // path traversal attempt (40 chars)
		"abc-2F-123--------------------------",     // would-be encoded slash (40 chars)
		"short",                                    // too short
		"abc123def456789012345678901234",           // 31 chars, not 40
		"ABCDEF1234567890123456789012345678901234", // uppercase not allowed
		"xyz-invalid-chars-1234567890123456789012", // non-hex chars
	}

	for _, invalidID := range invalidIDs {
		body := `{"body":"test"}`
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+invalidID+"/reply",
			strings.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		// Should not succeed - either 400 (validation) or 500 (internal error from invalid format)
		require.NotEqual(http.StatusCreated, rr.Code, "should reject invalid thread ID: %s", invalidID)
	}

	// Verify provider was never called with invalid IDs
	require.Empty(provider.replyToDiscussionCalls)
}

func TestResolveDiscussionE2E(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	dbRepo3, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)
	require.NotNil(dbRepo3)

	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         dbRepo3.ID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:          "Test MR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	// Valid 40-char hex thread ID
	threadID := "abc123def456789012345678901234567890abcd"
	body := `{"resolved":true}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+threadID+"/resolve",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	// Verify the provider was called with correct arguments
	require.Len(provider.resolveDiscussionCalls, 1)
	call := provider.resolveDiscussionCalls[0]
	assert.Equal(7, call.Number)
	assert.Equal(threadID, call.ThreadID)
	assert.True(call.Resolved)
	assert.Equal("acme", call.Ref.Owner)
	assert.Equal("widget", call.Ref.Name)
}

func TestDiscussionEndpointsRequireCapability(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	// Use the default test server which has GitHub provider (no discussion capabilities)
	srv, database := setupTestServer(t)

	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "github.com",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
		RepoPath:       "acme/widget",
	})
	require.NoError(err)

	_, err = database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         repoID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://github.com/acme/widget/pull/7",
		Title:          "Test PR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	threadID := "abc123def456789012345678901234567890abcd"

	// The default GitHub fixture does not expose GitLab discussion endpoints.
	body := `{"body":"test"}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/discussions/"+threadID+"/reply",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusConflict, rr.Code)

	// Resolve should also fail for GitHub
	body = `{"resolved":true}`
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/github/acme/widget/7/discussions/"+threadID+"/resolve",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusConflict, rr.Code)
}

func TestDiscussionEndpointsRejectNonExistentMR(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	// Note: We do NOT create an MR in the database, so MR #999 does not exist locally.
	threadID := "abc123def456789012345678901234567890abcd"

	// Reply should fail with 404 before calling provider
	body := `{"body":"test reply"}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/999/discussions/"+threadID+"/reply",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusNotFound, rr.Code)
	require.Empty(provider.replyToDiscussionCalls, "provider should not be called for non-existent MR")

	// Resolve should also fail with 404 before calling provider
	body = `{"resolved":true}`
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/999/discussions/"+threadID+"/resolve",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusNotFound, rr.Code)
	require.Empty(provider.resolveDiscussionCalls, "provider should not be called for non-existent MR")
}

func TestResolveDiscussionUpdatesLocalState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	database := dbtest.Open(t)

	ref := platform.RepoRef{
		Platform:           platform.KindGitLab,
		Host:               "gitlab.com",
		Owner:              "acme",
		Name:               "widget",
		RepoPath:           "acme/widget",
		PlatformID:         1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	provider := &gitLabDiscussionProvider{ref: ref}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)

	repo := ghclient.RepoRef{
		Platform:           platform.KindGitLab,
		Owner:              "acme",
		Name:               "widget",
		PlatformHost:       "gitlab.com",
		RepoPath:           "acme/widget",
		PlatformRepoID:     1234,
		PlatformExternalID: "gid://gitlab/Project/1234",
		WebURL:             "https://gitlab.com/acme/widget",
		CloneURL:           "https://gitlab.com/acme/widget.git",
		DefaultBranch:      "main",
	}

	syncer := ghclient.NewSyncerWithRegistry(
		registry, database, nil, []ghclient.RepoRef{repo}, time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)

	srv := servertest.New(t, database, syncer, nil, "/", nil, server.ServerOptions{})
	syncer.RunOnce(ctx)

	dbRepo, err := database.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     "gitlab",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)
	require.NotNil(dbRepo)

	collidingRepoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:     "github",
		PlatformHost: "gitlab.com",
		Owner:        "acme",
		Name:         "widget",
		RepoPath:     "acme/widget",
	})
	require.NoError(err)

	collidingMRID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         collidingRepoID,
		PlatformID:     9001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/pull/7",
		Title:          "Colliding PR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	mrID, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:         dbRepo.ID,
		PlatformID:     1001,
		Number:         7,
		URL:            "https://gitlab.com/acme/widget/-/merge_requests/7",
		Title:          "Test MR",
		Author:         "author",
		State:          "open",
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
		LastActivityAt: time.Now().UTC(),
	})
	require.NoError(err)

	// Create a discussion event that is NOT resolved
	threadID := "abc123def456789012345678901234567890abcd"
	platformID := int64(101)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: mrID,
		PlatformID:     &platformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "needs fix",
		CreatedAt:      time.Now().UTC(),
		DedupeKey:      "note-101",
		ThreadID:       &threadID,
		Resolvable:     true,
		Resolved:       false,
	}}))

	// Verify initial state
	events, err := database.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.False(events[0].Resolved)

	collidingPlatformID := int64(901)
	require.NoError(database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID: collidingMRID,
		PlatformID:     &collidingPlatformID,
		EventType:      "issue_comment",
		Author:         "reviewer",
		Body:           "same thread id in colliding repo",
		CreatedAt:      time.Now().UTC(),
		DedupeKey:      "colliding-note-901",
		ThreadID:       &threadID,
		Resolvable:     true,
		Resolved:       false,
	}}))

	// Resolve the discussion
	body := `{"resolved":true}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+threadID+"/resolve",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	// Verify the local state was updated
	events, err = database.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.True(events[0].Resolved, "local event should be marked as resolved")

	collidingEvents, err := database.ListMREvents(ctx, collidingMRID)
	require.NoError(err)
	require.Len(collidingEvents, 1)
	assert.False(collidingEvents[0].Resolved, "colliding provider event should not be updated")

	// Now unresolve it
	body = `{"resolved":false}`
	req = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/pulls/gitlab/acme/widget/7/discussions/"+threadID+"/resolve",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(http.StatusOK, rr.Code, "response: %s", rr.Body.String())

	// Verify the local state was updated back to unresolved
	events, err = database.ListMREvents(ctx, mrID)
	require.NoError(err)
	require.Len(events, 1)
	assert.False(events[0].Resolved, "local event should be marked as unresolved")
}
