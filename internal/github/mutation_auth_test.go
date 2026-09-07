package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/tokenauth"
)

// splitAuthReadIdentity and splitAuthWriteIdentity are the principals a
// split-auth route accounts to: App installation reads, user PAT writes.
var (
	splitAuthReadIdentity  = IdentityKey{Host: "github.example.com", Principal: "installation:11"}
	splitAuthWriteIdentity = IdentityKey{Host: "github.example.com", Principal: "user:1"}
)

// newSplitAuthTestClient builds a liveClient wired exactly like
// NewClient's read/write split (shared auth transport, mutation-marked
// write path) but pointed at srv instead of a real GitHub host.
func newSplitAuthTestClient(
	t *testing.T, srv *httptest.Server, source tokenauth.Source,
	registries ...*QuotaRegistry,
) *platformgithub.Client {
	t.Helper()
	options := []ClientOption{WithBaseURLForTesting(srv.URL)}
	if len(registries) > 0 {
		options = append(options, WithQuotaAccounting(
			registries[0], splitAuthReadIdentity, splitAuthWriteIdentity,
		))
	}
	client, err := NewClient(source, "github.com", nil, nil, options...)
	require.NoError(t, err)
	live, ok := client.(*platformgithub.Client)
	require.True(t, ok)
	return live
}

// TestMutationsUseUserPATWhileReadsUseAppToken pins the credential
// split at the wire level: with a github_app candidate ahead of the
// PAT in the chain, sync reads must authenticate with the minted
// installation token while user-facing writes (REST mutations and the
// ready-for-review GraphQL mutation) must carry the user's PAT so
// GitHub attributes them to the user, not "<app>[bot]".
func TestMutationsUseUserPATWhileReadsUseAppToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("TEST_SPLIT_AUTH_PAT", "user-pat")

	var mu sync.Mutex
	authByCall := map[string]string{}
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		authByCall[name] = r.Header.Get("Authorization")
	}

	var graphQLCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/releases",
		func(w http.ResponseWriter, r *http.Request) {
			record("read:releases", r)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-RateLimit-Limit", "15000")
			w.Header().Set("X-RateLimit-Remaining", "14999")
			w.Header().Set("X-RateLimit-Reset", "2000000000")
			_, _ = w.Write([]byte(`[]`))
		})
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/issues/5/comments",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:comment", r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets",
		func(w http.ResponseWriter, r *http.Request) {
			// Permissions are viewer-specific: only the PAT can push.
			if r.Header.Get("Authorization") == "Bearer user-pat" {
				record("repo:viewer-overlay", r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":1,"name":"widgets","permissions":{"push":true}}`))
				return
			}
			record("repo:metadata", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"name":"widgets","permissions":{"push":false}}`))
		})
	mux.HandleFunc("PUT /api/v3/repos/acme/widgets/pulls/5/merge-async",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:merge", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"merged",
				"details":{"message":"Pull request merged.","sha":"merge-sha"}
			}`))
		})
	mux.HandleFunc("PUT /api/v3/repos/acme/widgets/pulls/5/reviews/77/dismissals",
		func(w http.ResponseWriter, r *http.Request) {
			record("write:dismiss-review", r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":77,"state":"DISMISSED"}`))
		})
	mux.HandleFunc("POST /api/graphql",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Rate headers only on the node-ID lookup: the lookup
			// consumes the write credential's GraphQL budget too, and
			// the tracker must be fed even when the mutation response
			// carries no rate headers.
			if graphQLCalls.Load() == 0 {
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "4321")
				w.Header().Set("X-RateLimit-Reset", "2000000000")
			}
			if graphQLCalls.Add(1) == 1 {
				record("write:rfr-id-lookup", r)
				_, _ = w.Write([]byte(
					`{"data":{"repository":{"pullRequest":{"id":"PR_node"}}}}`,
				))
				return
			}
			record("write:rfr-mutation", r)
			_, _ = w.Write([]byte(
				`{"data":{"markPullRequestReadyForReview":{"pullRequest":{"number":5}}}}`,
			))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var mints atomic.Int64
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.com"},
		Candidates: []tokenauth.Candidate{
			{
				Kind:           tokenauth.SourceKindGitHubApp,
				Host:           "github.example.com",
				FilePath:       "/keys/app.pem",
				AppID:          7,
				InstallationID: 11,
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "TEST_SPLIT_AUTH_PAT"},
		},
	}, tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_app_token", time.Now().Add(time.Hour), nil
		},
	})
	quotaRegistry := NewQuotaRegistry()
	c := newSplitAuthTestClient(t, srv, source, quotaRegistry)
	writeGQLRT := NewRateTracker(openTestDB(t), "github.example.com", "user:1", "graphql_write")
	c.SetWriteGraphQLRateTracker(writeGQLRT)

	_, err := c.ListReleases(t.Context(), "acme", "widgets", 10)
	require.NoError(err)
	_, err = c.CreateIssueComment(t.Context(), "acme", "widgets", 5, "lgtm")
	require.NoError(err)
	_, err = c.MergePullRequest(t.Context(), "acme", "widgets", 5, "t", "m", "squash", "head-sha")
	require.NoError(err)
	_, err = c.MarkPullRequestReadyForReview(t.Context(), "acme", "widgets", 5)
	require.NoError(err)
	_, err = c.DismissReview(t.Context(), "acme", "widgets", 5, 77, "stale")
	require.NoError(err)
	// GetRepository reads metadata with the sync credential (so
	// app-only hosts keep syncing) and overlays the viewer-specific
	// permissions from the user's credential, which feed
	// viewer_can_merge.
	repo, err := c.GetRepository(t.Context(), "acme", "widgets")
	require.NoError(err)
	assert.True(repo.GetPermissions().GetPush(),
		"permissions must come from the PAT overlay, not the read-only app")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal("Bearer ghs_app_token", authByCall["read:releases"])
	assert.Equal("Bearer user-pat", authByCall["write:comment"])
	assert.Equal("Bearer user-pat", authByCall["write:merge"])
	assert.Equal("Bearer user-pat", authByCall["write:dismiss-review"])
	assert.Equal("Bearer user-pat", authByCall["write:rfr-id-lookup"])
	assert.Equal("Bearer user-pat", authByCall["write:rfr-mutation"])
	assert.Equal("Bearer ghs_app_token", authByCall["repo:metadata"],
		"repository metadata must stay on the sync credential")
	assert.Equal("Bearer user-pat", authByCall["repo:viewer-overlay"])
	assert.Equal(int64(1), mints.Load(),
		"reads share one minted token; writes must not mint")
	// Every write-credential GraphQL request feeds the write GraphQL
	// tracker, including the ready-for-review node-ID lookup — the
	// fake only sets rate headers on the lookup response.
	assert.Equal(4321, writeGQLRT.Remaining())
	appREST, ok := quotaRegistry.Get(splitAuthReadIdentity, QuotaResourceREST)
	require.True(ok)
	assert.Equal(14999, appREST.Remaining)
	userGraphQL, ok := quotaRegistry.Get(splitAuthWriteIdentity, QuotaResourceGraphQL)
	require.True(ok)
	assert.Equal(4321, userGraphQL.Remaining)
}

func TestNotificationAPIsUseUserAuthAndBackgroundBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("TEST_NOTIFICATION_AUTH_PAT", "user-pat")

	var mu sync.Mutex
	authByCall := map[string]string{}
	record := func(name string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		authByCall[name] = r.Header.Get("Authorization")
	}
	setRate := func(w http.ResponseWriter, remaining string) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", remaining)
		w.Header().Set("X-RateLimit-Reset", "2000000000")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/notifications",
		func(w http.ResponseWriter, r *http.Request) {
			record("notifications:list-repo", r)
			setRate(w, "4990")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{
				"id":"ntf-1",
				"unread":true,
				"reason":"mention",
				"updated_at":"2026-06-17T00:00:00Z",
				"repository":{"name":"widgets","owner":{"login":"acme"}},
				"subject":{"title":"Review","type":"PullRequest","url":"https://github.example.com/api/v3/repos/acme/widgets/pulls/5"}
			}]`))
		})
	mux.HandleFunc("GET /api/v3/notifications/threads/ntf-1",
		func(w http.ResponseWriter, r *http.Request) {
			record("notifications:get", r)
			setRate(w, "4989")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"ntf-1",
				"unread":true,
				"reason":"mention",
				"updated_at":"2026-06-17T00:00:00Z",
				"repository":{"name":"widgets","owner":{"login":"acme"}},
				"subject":{"title":"Review","type":"PullRequest","url":"https://github.example.com/api/v3/repos/acme/widgets/pulls/5"}
			}`))
		})
	mux.HandleFunc("PATCH /api/v3/notifications/threads/ntf-1",
		func(w http.ResponseWriter, r *http.Request) {
			record("notifications:mark-read", r)
			setRate(w, "4988")
			w.WriteHeader(http.StatusNoContent)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var mints atomic.Int64
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.example.com"},
		Candidates: []tokenauth.Candidate{
			{
				Kind:           tokenauth.SourceKindGitHubApp,
				Host:           "github.example.com",
				FilePath:       "/keys/app.pem",
				AppID:          7,
				InstallationID: 11,
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "TEST_NOTIFICATION_AUTH_PAT"},
		},
	}, tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			mints.Add(1)
			return "ghs_app_token", time.Now().Add(time.Hour), nil
		},
	})
	database := openTestDB(t)
	readRT := NewRateTracker(database, "github.example.com", "installation:11", "rest")
	writeRT := NewRateTracker(database, "github.example.com", "user:1", "rest")
	readBudget := NewSyncBudget(100)
	writeBudget := NewSyncBudget(100)
	quotaRegistry := NewQuotaRegistry()
	client, err := NewClient(
		source,
		"github.example.com",
		readRT,
		readBudget,
		WithBaseURLForTesting(srv.URL),
		WithNotificationAccounting(writeRT, writeBudget),
		WithQuotaAccounting(
			quotaRegistry, splitAuthReadIdentity, splitAuthWriteIdentity,
		),
	)
	require.NoError(err)
	c, ok := client.(*platformgithub.Client)
	require.True(ok)

	syncCtx := WithSyncBudget(t.Context())
	threads, hasNext, err := c.ListNotifications(syncCtx, NotificationListOptions{
		All:       true,
		RepoOwner: "acme",
		RepoName:  "widgets",
	})
	require.NoError(err)
	require.False(hasNext)
	require.Len(threads, 1)
	_, err = c.GetNotificationThread(syncCtx, "ntf-1")
	require.NoError(err)
	require.NoError(c.MarkNotificationThreadRead(syncCtx, "ntf-1"))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal("Bearer user-pat", authByCall["notifications:list-repo"])
	assert.Equal("Bearer user-pat", authByCall["notifications:get"])
	assert.Equal("Bearer user-pat", authByCall["notifications:mark-read"])
	assert.Equal(int64(0), mints.Load(), "notification APIs must not mint app tokens")
	assert.Equal(0, readRT.RequestsThisHour())
	assert.Equal(-1, readRT.Remaining(),
		"PAT notification responses must not overwrite the app-token read tracker")
	assert.Equal(3, writeRT.RequestsThisHour())
	assert.Equal(4988, writeRT.Remaining())
	assert.Equal(0, readBudget.Spent())
	assert.Equal(3, writeBudget.Spent())
	// Notification traffic authenticates with the PAT, so its quota must
	// land on the write principal and leave the App installation pool
	// untouched.
	userPool, ok := quotaRegistry.Get(splitAuthWriteIdentity, QuotaResourceREST)
	require.True(ok)
	assert.Equal(4988, userPool.Remaining)
	_, appPoolExists := quotaRegistry.Get(splitAuthReadIdentity, QuotaResourceREST)
	assert.False(appPoolExists)
}

// TestViewerPermissionOverlayChargesWriteBudget pins background split-auth
// accounting: the GetRepository viewer overlay runs on the write credential
// during sync, so it must spend the write identity's sync budget, while
// foreground mutations on the same transport stay uncharged.
func TestViewerPermissionOverlayChargesWriteBudget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("TEST_OVERLAY_BUDGET_PAT", "user-pat")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/acme/widgets",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer user-pat" {
				_, _ = w.Write([]byte(`{"id":1,"name":"widgets","permissions":{"push":true}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":1,"name":"widgets","permissions":{"push":false}}`))
		})
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/issues/5/comments",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.example.com"},
		Candidates: []tokenauth.Candidate{
			{
				Kind:           tokenauth.SourceKindGitHubApp,
				Host:           "github.example.com",
				FilePath:       "/keys/app.pem",
				AppID:          7,
				InstallationID: 11,
			},
			{Kind: tokenauth.SourceKindEnv, EnvName: "TEST_OVERLAY_BUDGET_PAT"},
		},
	}, tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_app_token", time.Now().Add(time.Hour), nil
		},
	})
	readBudget := NewSyncBudget(100)
	writeBudget := NewSyncBudget(100)
	client, err := NewClient(
		source, "github.example.com", nil, readBudget,
		WithBaseURLForTesting(srv.URL),
		WithNotificationAccounting(nil, writeBudget),
	)
	require.NoError(err)

	repo, err := client.GetRepository(WithSyncBudget(t.Context()), "acme", "widgets")
	require.NoError(err)
	assert.True(repo.GetPermissions().GetPush())
	assert.Equal(1, readBudget.Spent())
	assert.Equal(1, writeBudget.Spent(),
		"the background viewer overlay must spend the write identity's budget")

	_, err = client.CreateIssueComment(t.Context(), "acme", "widgets", 5, "lgtm")
	require.NoError(err)
	assert.Equal(1, writeBudget.Spent(), "foreground mutations stay uncharged")
	assert.Equal(1, readBudget.Spent())

	// Archive requests hold leases for the read identity only: the viewer
	// overlay is skipped so the write credential is never spent, and the
	// app's permissions are cleared rather than persisted.
	repo, err = client.GetRepository(WithArchiveSyncBudget(t.Context()), "acme", "widgets")
	require.NoError(err)
	assert.Nil(repo.Permissions)
	assert.Equal(1, writeBudget.Spent(),
		"archive repository reads must not spend the write identity")
	assert.Equal(2, readBudget.Spent())
}

// TestMutationAuthFallsBackToReadClientWhenUnsplit pins the hand-built
// client shape used across this package's tests: without a dedicated
// write client, mutations flow through the read client unchanged.
func TestNewClientRejectsMutationsWithoutStartupWriteIdentity(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	client, err := NewClient(
		testTokenSource("late-pat"), "github.example.com", nil, nil,
		WithBaseURLForTesting(server.URL), WithMutationsDisabled(),
	)
	require.NoError(err)

	_, err = client.CreateIssueComment(t.Context(), "acme", "widget", 1, "body")
	require.ErrorIs(err, ErrMissingWriteIdentity)
	_, _, err = client.ListNotifications(t.Context(), NotificationListOptions{
		RepoOwner: "acme", RepoName: "widget",
	})
	require.ErrorIs(err, ErrMissingWriteIdentity)
}

func TestMutationAuthFallsBackToReadClientWhenUnsplit(t *testing.T) {
	require := require.New(t)
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/issues/5/comments",
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("TEST_SPLIT_AUTH_PAT", "only-pat")
	source := tokenauth.NewManagedSource(tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.example.com"},
		Candidates: []tokenauth.Candidate{
			{Kind: tokenauth.SourceKindEnv, EnvName: "TEST_SPLIT_AUTH_PAT"},
		},
	}, tokenauth.Options{})
	authRT := platform.AuthTransport{
		Source:    source,
		Base:      http.DefaultTransport,
		SetHeader: platform.BearerAuthHeader,
	}
	ghClient, err := newEnterpriseGHClient(&http.Client{Transport: authRT},
		srv.URL+"/api/v3/", srv.URL+"/api/uploads/")

	require.NoError(err)
	c, err := platformgithub.NewClient(platformgithub.ClientConfig{
		Host: "github.example.com", Read: ghClient.Client(), Write: ghClient.Client(), Notifications: ghClient.Client(),
		Clock: time.Now, APIBase: ghClient.BaseURL(),
	})
	require.NoError(err)

	_, err = c.CreateIssueComment(t.Context(), "acme", "widgets", 5, "hello")
	require.NoError(err)
	assert.Equal(t, "Bearer only-pat", gotAuth)
}
