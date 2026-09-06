package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/ratelimit"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/pullapi"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/internal/tokenauth"
)

func TestDeriveOperationAvailability(t *testing.T) {
	allCaps := httpapi.ProviderCapabilitiesResponse{
		ReadRepositories:            true,
		ReadMergeRequests:           true,
		ReadIssues:                  true,
		ReadComments:                true,
		ReadReleases:                true,
		ReadCI:                      true,
		ReadLabels:                  true,
		CommentMutation:             true,
		StateMutation:               true,
		MergeMutation:               true,
		ReviewMutation:              true,
		ReviewDraftMutation:         true,
		WorkflowApproval:            true,
		WorkflowDispatch:            true,
		ReadyForReview:              true,
		DraftMutation:               true,
		IssueMutation:               true,
		LabelMutation:               true,
		ReviewSuggestionApplication: true,
		MutationHeadBinding:         true,
		ReadReviewThreads:           true,
	}
	mergePR := operationDescriptor{
		name:                 operationMergePR,
		requiredCapabilities: []string{capabilityMergeMutation},
	}
	submitReview := operationDescriptor{
		name:                 operationSubmitReview,
		requiredCapabilities: []string{capabilityReviewMutation},
	}
	addLabel := operationDescriptor{
		name:                 operationAddLabel,
		requiredCapabilities: []string{capabilityReadLabels, capabilityLabelMutation},
	}
	resetAt := time.Date(2026, 5, 19, 14, 35, 0, 0, time.UTC)
	limitedRate := rateLimitAvailability{
		limited: true,
		reason:  "github.com rate-limited",
		retryAt: resetAt.UTC().Format(time.RFC3339),
	}
	repoCanMerge := db.Repo{ViewerCanMerge: true}
	repoCannotMerge := db.Repo{ViewerCanMerge: false}

	tests := []struct {
		name      string
		op        operationDescriptor
		caps      httpapi.ProviderCapabilitiesResponse
		repo      db.Repo
		rate      rateLimitAvailability
		writeCred writeCredentialGate
		opContext operationAvailabilityContext
		expected  httpapi.OperationAvailability
	}{
		{
			name:     "healthy merge_pr is available",
			op:       mergePR,
			caps:     allCaps,
			repo:     repoCanMerge,
			expected: httpapi.OperationAvailability{Available: true},
		},
		{
			name: "dispatch_workflow is unavailable without workflow_dispatch",
			op:   descDispatchWorkflow,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.WorkflowDispatch = false
				return c
			}(),
			repo: repoCanMerge,
			expected: httpapi.OperationAvailability{
				Code:               availabilityCodeUnsupportedCapability,
				UnavailableReason:  "Provider does not support workflow_dispatch",
				RequiredCapability: capabilityWorkflowDispatch,
			},
		},
		{
			name: "dispatch_workflow is unavailable without a write credential",
			op:   descDispatchWorkflow,
			caps: allCaps,
			repo: repoCanMerge,
			writeCred: writeCredentialGate{
				code:   availabilityCodeMissingWriteCredential,
				reason: "No user credential for writes on github.com",
			},
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeMissingWriteCredential,
				UnavailableReason: "No user credential for writes on github.com",
			},
		},
		{
			name: "dispatch_workflow is unavailable during a REST rate-limit window",
			op:   descDispatchWorkflow,
			caps: allCaps,
			repo: repoCanMerge,
			rate: operationRateLimitForBuckets(
				descDispatchWorkflow.rateLimitBuckets(),
				map[apiBucket]rateLimitAvailability{apiBucketREST: limitedRate},
			),
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeRateLimited,
				UnavailableReason: "github.com rate-limited",
				RetryAt:           resetAt.UTC().Format(time.RFC3339),
			},
		},
		{
			name:     "dispatch_workflow is available when all gates pass",
			op:       descDispatchWorkflow,
			caps:     allCaps,
			repo:     repoCanMerge,
			expected: httpapi.OperationAvailability{Available: true},
		},
		{
			name: "missing required capability surfaces unsupported_capability",
			op:   mergePR,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.MergeMutation = false
				return c
			}(),
			repo: repoCanMerge,
			expected: httpapi.OperationAvailability{
				Code:               availabilityCodeUnsupportedCapability,
				UnavailableReason:  "Provider does not support merge_mutation",
				RequiredCapability: capabilityMergeMutation,
			},
		},
		{
			name: "first missing capability wins for multi-cap operations",
			op:   addLabel,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.ReadLabels = false
				c.LabelMutation = false
				return c
			}(),
			repo: repoCanMerge,
			expected: httpapi.OperationAvailability{
				Code:               availabilityCodeUnsupportedCapability,
				UnavailableReason:  "Provider does not support read_labels",
				RequiredCapability: capabilityReadLabels,
			},
		},
		{
			name: "viewer without merge permission cannot merge even when capability exists",
			op:   mergePR,
			caps: allCaps,
			repo: repoCannotMerge,
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeViewerCannotMerge,
				UnavailableReason: "You do not have permission to merge in this repository",
			},
		},
		{
			name:     "viewer_can_merge gate is scoped to merge_pr only",
			op:       operationDescriptor{name: operationClosePR, requiredCapabilities: []string{capabilityStateMutation}},
			caps:     allCaps,
			repo:     repoCannotMerge,
			expected: httpapi.OperationAvailability{Available: true},
		},
		{
			name:      "viewer cannot approve own pull request",
			op:        submitReview,
			caps:      allCaps,
			repo:      repoCanMerge,
			opContext: operationAvailabilityContext{selfApproval: true},
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeSelfApproval,
				UnavailableReason: "You cannot approve your own pull request",
			},
		},
		{
			name: "review draft operation uses draft capability without requiring submitted reviews",
			op:   descReviewDraft,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.ReviewMutation = false
				c.ReviewDraftMutation = true
				return c
			}(),
			repo:     repoCanMerge,
			expected: httpapi.OperationAvailability{Available: true},
		},
		{
			name:     "review suggestion operation requires stored thread and head binding prerequisites",
			op:       descApplyReviewSuggestion,
			caps:     allCaps,
			repo:     repoCanMerge,
			expected: httpapi.OperationAvailability{Available: true},
		},
		{
			name: "first missing review suggestion prerequisite wins",
			op:   descApplyReviewSuggestion,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.MutationHeadBinding = false
				c.ReadReviewThreads = false
				return c
			}(),
			repo: repoCanMerge,
			expected: httpapi.OperationAvailability{
				Code:               availabilityCodeUnsupportedCapability,
				UnavailableReason:  "Provider does not support mutation_head_binding",
				RequiredCapability: capabilityMutationHeadBinding,
			},
		},
		{
			name: "rate-limited host blocks even when capability and permission exist",
			op:   mergePR,
			caps: allCaps,
			repo: repoCanMerge,
			rate: limitedRate,
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeRateLimited,
				UnavailableReason: "github.com rate-limited",
				RetryAt:           resetAt.UTC().Format(time.RFC3339),
			},
		},
		{
			name: "unsupported capability takes precedence over rate limit",
			op:   mergePR,
			caps: func() httpapi.ProviderCapabilitiesResponse {
				c := allCaps
				c.MergeMutation = false
				return c
			}(),
			repo: repoCanMerge,
			rate: limitedRate,
			expected: httpapi.OperationAvailability{
				Code:               availabilityCodeUnsupportedCapability,
				UnavailableReason:  "Provider does not support merge_mutation",
				RequiredCapability: capabilityMergeMutation,
			},
		},
		{
			name: "missing write credential blocks every mutation",
			op:   mergePR,
			caps: allCaps,
			repo: repoCanMerge,
			writeCred: writeCredentialGate{
				code:   availabilityCodeMissingWriteCredential,
				reason: "No user credential for writes on github.com",
			},
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeMissingWriteCredential,
				UnavailableReason: "No user credential for writes on github.com",
			},
		},
		{
			name: "write credential gate takes precedence over viewer and rate gates",
			op:   mergePR,
			caps: allCaps,
			repo: repoCannotMerge,
			rate: limitedRate,
			writeCred: writeCredentialGate{
				code:   availabilityCodeWriteCredentialError,
				reason: "Resolving the write credential for github.com failed",
			},
			expected: httpapi.OperationAvailability{
				Code:              availabilityCodeWriteCredentialError,
				UnavailableReason: "Resolving the write credential for github.com failed",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveOperationAvailabilityWithContext(
				tc.op, tc.caps, tc.repo, tc.rate, tc.writeCred, tc.opContext,
			)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestOperationRateLimitChecksAllBuckets(t *testing.T) {
	resetAt := time.Date(2026, 5, 19, 14, 35, 0, 0, time.UTC)
	restRate := rateLimitAvailability{
		limited: true,
		reason:  "github.com REST rate-limited",
		retryAt: resetAt.UTC().Format(time.RFC3339),
	}
	graphQLRate := rateLimitAvailability{
		limited: true,
		reason:  "github.com GraphQL rate-limited",
		retryAt: resetAt.Add(time.Minute).UTC().Format(time.RFC3339),
	}
	multiBucketOp := operationDescriptor{
		bucket:       apiBucketREST,
		extraBuckets: []apiBucket{apiBucketGraphQL},
	}

	assert.Equal(t, restRate, operationRateLimitForBuckets(multiBucketOp.rateLimitBuckets(), map[apiBucket]rateLimitAvailability{
		apiBucketREST:    restRate,
		apiBucketGraphQL: graphQLRate,
	}))
	assert.Equal(t, graphQLRate, operationRateLimitForBuckets(multiBucketOp.rateLimitBuckets(), map[apiBucket]rateLimitAvailability{
		apiBucketGraphQL: graphQLRate,
	}))
	assert.Equal(t, []apiBucket{apiBucketREST}, descApplyReviewSuggestion.rateLimitBuckets())
}

func TestFormatRateLimit(t *testing.T) {
	assert := assert.New(t)

	resetAt := time.Date(2026, 5, 19, 14, 35, 0, 0, time.UTC)
	got := formatRateLimit("github.com", &resetAt)
	assert.True(got.limited)
	assert.Equal("github.com rate-limited", got.reason)
	assert.Equal(resetAt.UTC().Format(time.RFC3339), got.retryAt)

	unknown := formatRateLimit("ghe.example.com", nil)
	assert.True(unknown.limited)
	assert.Equal("ghe.example.com rate-limited", unknown.reason)
	assert.Empty(unknown.retryAt)
}

func TestRepoOperationsWireShape(t *testing.T) {
	// The set of operation field names on httpapi.RepoOperations is a wire
	// contract. Renaming a json tag here breaks any frontend pinned
	// to an older schema, so the test enumerates the full set as a
	// guard against accidental renames.
	require := require.New(t)
	fields := reflect.VisibleFields(reflect.TypeFor[httpapi.RepoOperations]())
	tags := make([]string, 0, len(fields))
	for _, f := range fields {
		tag := f.Tag.Get("json")
		require.NotEmpty(tag, "field %s missing json tag", f.Name)
		tags = append(tags, tag)
	}
	require.Equal([]string{
		"merge_pr",
		"close_pr",
		"reopen_pr",
		"mark_ready_for_review",
		"mark_draft",
		"submit_review",
		"review_draft",
		"add_comment",
		"edit_comment",
		"delete_comment",
		"add_label",
		"remove_label",
		"set_assignees",
		"set_reviewers",
		"create_issue",
		"close_issue",
		"reopen_issue",
		"approve_workflow",
		"dispatch_workflow",
		"update_content",
		"reply_review_thread",
		"resolve_review_thread",
		"apply_review_suggestion",
	}, tags)
}

// newServerWithRateTracker builds a Server whose syncer is wired
// with a single REST rate tracker for github.com so tests can flip
// the host into the paused state by calling UpdateFromRate.
func newServerWithRateTracker(t *testing.T) (*Server, *db.DB, *ratelimit.RateTracker) {
	t.Helper()
	database := dbtest.Open(t)
	rt := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockGH{}},
		database, nil,
		[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*ghclient.RateTracker{ratelimit.RateBucketKey("github", "github.com", "host"): rt},
		nil,
	)
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	return srv, database, rt
}

func TestAPIRepoResponseIncludesOperationsHealthy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database, _ := newServerWithRateTracker(t)
	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	// Keep merge available so this fixture isolates the healthy path.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repoID, true))

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)

	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

	merge := resp.Operations.MergePR
	assert.True(merge.Available)
	assert.Empty(merge.Code)
	assert.Empty(merge.UnavailableReason)
}

func TestAPIRepoResponseIncludesOperationsRateLimited(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database, rt := newServerWithRateTracker(t)
	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	// Keep merge available so this fixture isolates rate limiting.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repoID, true))

	resetAt := time.Now().UTC().Add(30 * time.Minute)
	rt.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)

	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

	merge := resp.Operations.MergePR
	assert.False(merge.Available)
	assert.Equal(availabilityCodeRateLimited, merge.Code)
	assert.Contains(merge.UnavailableReason, "rate-limited")
	assert.NotEmpty(merge.RetryAt)
}

func TestAPIRepoResponseIncludesOperationsGraphQLPauseDoesNotBlockREST(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Mutations in kenn-forge are REST-backed, so a paused GraphQL
	// tracker must leave merge_pr available; this guards against
	// the earlier behavior that treated either tracker's pause as
	// blocking every operation.
	database := dbtest.Open(t)
	restRT := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	gqlRT := ghclient.NewRateTracker(database, "github.com", "host", "graphql")
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockGH{}},
		database, nil,
		[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*ghclient.RateTracker{ratelimit.RateBucketKey("github", "github.com", "host"): restRT},
		nil,
	)
	syncer.SetFetchers(map[string]*ghclient.GraphQLFetcher{
		"github.com": ghclient.NewGraphQLFetcher(
			testTokenSource("fake-token"), "github.com", gqlRT, nil,
		),
	})
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	// Keep merge available so this fixture isolates GraphQL tracker state.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repoID, true))

	// Pause GraphQL by reporting zero remaining requests with a
	// future reset; leave REST untouched.
	resetAt := time.Now().UTC().Add(30 * time.Minute)
	gqlRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)

	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

	merge := resp.Operations.MergePR
	assert.True(merge.Available, "merge_pr is REST-backed; GraphQL pause must not block it")
	assert.Empty(merge.Code)
}

func TestAPIRepoResponseApplySuggestionRateBucketsFollowProvider(t *testing.T) {
	resetAt := time.Now().UTC().Add(30 * time.Minute)

	t.Run("github reports rest and graphql apply suggestion buckets", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		database := dbtest.Open(t)
		gqlRT := ghclient.NewRateTracker(database, "github.com", "host", "graphql")
		key := ratelimit.RateBucketKey("github", "github.com", "host")
		syncer := ghclient.NewSyncer(
			map[string]ghclient.Client{"github.com": &mockGH{}},
			database, nil,
			[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
			time.Minute,
			map[string]*ghclient.RateTracker{
				key: ghclient.NewRateTracker(database, "github.com", "host", "rest"),
			},
			nil,
		)
		syncer.SetFetchers(map[string]*ghclient.GraphQLFetcher{
			"github.com": ghclient.NewGraphQLFetcherWithClient(nil, gqlRT),
		})
		t.Cleanup(syncer.Stop)
		srv := New(database, syncer, nil, "/", nil, ServerOptions{})
		t.Cleanup(func() { gracefulShutdown(t, srv) })

		_, err := database.UpsertRepo(
			t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
		)
		require.NoError(err)
		gqlRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

		rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
		require.Equal(http.StatusOK, rr.Code)
		var resp repoResponse
		require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

		suggestion := resp.Operations.ApplyReviewSuggestion
		assert.False(suggestion.Available)
		assert.Equal(availabilityCodeRateLimited, suggestion.Code)
	})

	t.Run("provider without bucket hook keeps descriptor default", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		database := dbtest.Open(t)
		gqlRT := ghclient.NewPlatformRateTracker(database, "gitlab", "gitlab.example.com", "host", "graphql")
		provider := &apiTestGitLabProvider{
			ref: platform.RepoRef{
				Platform: platform.KindGitLab,
				Host:     "gitlab.example.com",
				Owner:    "group",
				Name:     "project",
				RepoPath: "group/project",
			},
			capabilities: &platform.Capabilities{
				ReadRepositories:            true,
				ReadMergeRequests:           true,
				ReviewSuggestionApplication: true,
				MutationHeadBinding:         true,
				ReadReviewThreads:           true,
			},
		}
		registry, err := platform.NewRegistry(provider)
		require.NoError(err)
		syncer := ghclient.NewSyncerWithRegistry(
			registry, database, nil,
			[]ghclient.RepoRef{{
				Platform:     platform.KindGitLab,
				PlatformHost: "gitlab.example.com",
				Owner:        "group",
				Name:         "project",
				RepoPath:     "group/project",
			}},
			time.Minute,
			nil,
			nil,
		)
		syncer.SetFetchers(map[string]*ghclient.GraphQLFetcher{
			"gitlab.example.com": ghclient.NewGraphQLFetcherWithClient(nil, gqlRT),
		})
		t.Cleanup(syncer.Stop)
		srv := New(database, syncer, nil, "/", nil, ServerOptions{})
		t.Cleanup(func() { gracefulShutdown(t, srv) })

		_, err = database.UpsertRepo(t.Context(), db.RepoIdentity{
			Platform:       "gitlab",
			PlatformHost:   "gitlab.example.com",
			PlatformRepoID: "gid://gitlab/Project/42",
			RepoPath:       "group/project",
		})
		require.NoError(err)
		gqlRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

		rr := testutil.DoJSON(
			t, srv, http.MethodGet,
			"/api/v1/host/gitlab.example.com/repo/gitlab/group/project", nil)

		require.Equal(http.StatusOK, rr.Code)
		var resp repoResponse
		require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

		suggestion := resp.Operations.ApplyReviewSuggestion
		assert.True(suggestion.Available)
		assert.Empty(suggestion.Code)
	})

	assertInvalidProviderBuckets := func(t *testing.T, buckets []platform.RateLimitBucket) {
		t.Helper()
		require := require.New(t)
		assert := assert.New(t)
		database := dbtest.Open(t)
		provider := &apiTestGitLabProvider{
			ref: platform.RepoRef{
				Platform: platform.KindGitLab,
				Host:     "gitlab.example.com",
				Owner:    "group",
				Name:     "project",
				RepoPath: "group/project",
			},
			capabilities: &platform.Capabilities{
				ReadRepositories:            true,
				ReadMergeRequests:           true,
				ReviewSuggestionApplication: true,
				MutationHeadBinding:         true,
				ReadReviewThreads:           true,
			},
			rateLimitBuckets: map[platform.OperationName][]platform.RateLimitBucket{
				platform.OperationApplyReviewSuggestion: buckets,
			},
		}
		registry, err := platform.NewRegistry(provider)
		require.NoError(err)
		syncer := ghclient.NewSyncerWithRegistry(
			registry, database, nil,
			[]ghclient.RepoRef{{
				Platform:     platform.KindGitLab,
				PlatformHost: "gitlab.example.com",
				Owner:        "group",
				Name:         "project",
				RepoPath:     "group/project",
			}},
			time.Minute,
			nil,
			nil,
		)
		t.Cleanup(syncer.Stop)
		srv := New(database, syncer, nil, "/", nil, ServerOptions{})
		t.Cleanup(func() { gracefulShutdown(t, srv) })

		_, err = database.UpsertRepo(t.Context(), db.RepoIdentity{
			Platform:       "gitlab",
			PlatformHost:   "gitlab.example.com",
			PlatformRepoID: "gid://gitlab/Project/42",
			RepoPath:       "group/project",
		})
		require.NoError(err)

		rr := testutil.DoJSON(
			t, srv, http.MethodGet,
			"/api/v1/host/gitlab.example.com/repo/gitlab/group/project", nil)

		require.Equal(http.StatusOK, rr.Code)
		var resp repoResponse
		require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

		suggestion := resp.Operations.ApplyReviewSuggestion
		assert.False(suggestion.Available)
		assert.Equal(availabilityCodeRateLimited, suggestion.Code)
		assert.Contains(suggestion.UnavailableReason, "invalid rate-limit buckets")
	}

	t.Run("empty provider bucket report fails closed", func(t *testing.T) {
		assertInvalidProviderBuckets(t, []platform.RateLimitBucket{})
	})

	t.Run("unknown provider bucket report fails closed", func(t *testing.T) {
		assertInvalidProviderBuckets(t, []platform.RateLimitBucket{platform.RateLimitBucket("restt")})
	})
}

func TestAPIRepoResponseOperationsGateOnWriteTrackerWhenSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// With a GitHub App handling sync reads, mutations ride the user's
	// PAT and must gate on the write credential's budget: an exhausted
	// app tracker must not disable PAT-backed writes, and an exhausted
	// write tracker must disable them even while reads still flow.
	database := dbtest.Open(t)
	restRT := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	writeRT := ghclient.NewRateTracker(database, "github.com", "user:1", "rest_write")
	writeGQLRT := ghclient.NewRateTracker(database, "github.com", "user:1", "graphql_write")
	key := ratelimit.RateBucketKey("github", "github.com", "host")
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": &mockGH{}},
		database, nil,
		[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute,
		map[string]*ghclient.RateTracker{key: restRT},
		nil,
	)
	syncer.SetWriteRateTrackers(map[string]*ghclient.RateTracker{key: writeRT})
	syncer.SetWriteGQLRateTrackers(map[string]*ghclient.RateTracker{key: writeGQLRT})
	t.Cleanup(syncer.Stop)
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })

	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	// Keep merge available so this fixture isolates write tracker state.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repoID, true))

	// App (sync) budget exhausted, PAT healthy: writes stay available.
	resetAt := time.Now().UTC().Add(30 * time.Minute)
	restRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(resp.Operations.MergePR.Available,
		"app budget exhaustion must not disable PAT-backed writes")

	// PAT REST budget exhausted: REST writes gate, but the GraphQL
	// mutation (ready-for-review) follows its own write bucket.
	writeRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

	rr = testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	resp = repoResponse{}
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	merge := resp.Operations.MergePR
	assert.False(merge.Available, "PAT exhaustion must disable writes")
	assert.Equal(availabilityCodeRateLimited, merge.Code)
	assert.True(resp.Operations.MarkReadyForReview.Available,
		"REST write exhaustion must not gate the GraphQL-backed mutation")
	assert.True(resp.Operations.MarkDraft.Available,
		"REST write exhaustion must not gate GraphQL-backed draft conversion")

	// PAT GraphQL budget exhausted: only the GraphQL mutation gates.
	writeRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 4000, Reset: resetAt})
	writeGQLRT.UpdateFromRate(ratelimit.Rate{Limit: 5000, Remaining: 0, Reset: resetAt})

	rr = testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	resp = repoResponse{}
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.True(resp.Operations.MergePR.Available)
	rfr := resp.Operations.MarkReadyForReview
	assert.False(rfr.Available, "write GraphQL exhaustion must gate ready-for-review")
	assert.Equal(availabilityCodeRateLimited, rfr.Code)
	draft := resp.Operations.MarkDraft
	assert.False(draft.Available, "write GraphQL exhaustion must gate draft conversion")
	assert.Equal(availabilityCodeRateLimited, draft.Code)
}

func TestAPIRepoResponseOperationsRequireWriteCredentialWhenSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// A host can sync with only the GitHub App configured, but
	// mutations skip the app candidate so they stay attributed to the
	// user. With no PAT or gh credential behind the app, every write
	// operation must report missing_write_credential instead of
	// offering an action that would fail auth at request time.
	t.Setenv("SPLIT_WRITE_CRED_PAT", "")
	t.Setenv("SPLIT_WRITE_CRED_PAT_NEW", "user-pat")
	srv, set := newSplitTestServer(t, tokenauth.Candidate{
		Kind: tokenauth.SourceKindEnv, EnvName: "SPLIT_WRITE_CRED_PAT",
	})
	router, err := ghclient.NewHostRouter(
		"github.com",
		&ghclient.Route{
			Key: ghclient.RouteKey{Host: "github.com", Owner: "acme"},
			ReadIdentity: ghclient.IdentityKey{
				Host: "github.com", Principal: "installation:11",
			},
		},
	)
	require.NoError(err)
	srv.syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	merge := resp.Operations.MergePR
	assert.False(merge.Available, "app-only host has no credential for mutations")
	assert.Equal(availabilityCodeMissingWriteCredential, merge.Code)
	assert.Contains(merge.UnavailableReason, "github.com")
	comment := resp.Operations.AddComment
	assert.False(comment.Available, "every operation is a mutation; all must gate")
	assert.Equal(availabilityCodeMissingWriteCredential, comment.Code)

	// Identity assignment is restart-bound. A token appearing behind an
	// App-only route cannot silently move writes onto a new user principal in
	// the running process, even though the source itself reloads successfully.
	set.Upsert(splitTestDescriptor(tokenauth.Candidate{
		Kind: tokenauth.SourceKindEnv, EnvName: "SPLIT_WRITE_CRED_PAT_NEW",
	}))
	rr = testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	resp = repoResponse{}
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	assert.False(resp.Operations.MergePR.Available)
	assert.Equal(availabilityCodeMissingWriteCredential, resp.Operations.MergePR.Code)
	assert.False(resp.Operations.AddComment.Available)
}

func TestAPIRepoResponseOperationsDistinguishWriteCredentialErrors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// A resolver failure (unreadable token file, broken gh helper) is
	// not a missing credential: the UI must not tell the user to
	// configure a PAT when the real problem is a broken helper.
	srv, _ := newSplitTestServer(t, tokenauth.Candidate{
		Kind:     tokenauth.SourceKindFile,
		FilePath: filepath.Join(t.TempDir(), "does-not-exist.token"),
	})

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	merge := resp.Operations.MergePR
	assert.False(merge.Available)
	assert.Equal(availabilityCodeWriteCredentialError, merge.Code,
		"a resolver failure must not masquerade as a missing credential")
	assert.Contains(merge.UnavailableReason, "github.com")
	assert.NotContains(merge.UnavailableReason, "does-not-exist.token",
		"the reason must stay redacted; no raw resolver error in the wire response")
}

type writeCredentialProbeClient struct {
	*mockGH
	err   error
	calls int
}

func (c *writeCredentialProbeClient) ProbeWriteCredential(context.Context) error {
	c.calls++
	return c.err
}

func TestAPIRepoResponseProbesRestartBoundWriteCredential(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	client := &writeCredentialProbeClient{mockGH: &mockGH{}, err: ghclient.ErrIdentityChanged}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": client}, database, nil,
		[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key: ghclient.RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity:  ghclient.IdentityKey{Host: "github.com", Principal: "installation:11"},
		WriteIdentity: ghclient.IdentityKey{Host: "github.com", Principal: "user:22"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	_, err = database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	merge := resp.Operations.MergePR
	assert.False(merge.Available)
	assert.Equal(availabilityCodeWriteCredentialError, merge.Code)
	assert.Contains(merge.UnavailableReason, "restart")

	rr = testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	assert.Equal(1, client.calls, "routed write credential probes must use the TTL cache")
}

func TestAPIRepoResponseDisablesWritesWhenConfiguredRouterHasNoRoute(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	database := dbtest.Open(t)
	client := &mockGH{}
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": client}, database, nil,
		[]ghclient.RepoRef{{Owner: "other", Name: "widget", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	router, err := ghclient.NewHostRouter("github.com", &ghclient.Route{
		Key: ghclient.RouteKey{Host: "github.com", Owner: "acme"}, Client: client,
		ReadIdentity:  ghclient.IdentityKey{Host: "github.com", Principal: "user:11"},
		WriteIdentity: ghclient.IdentityKey{Host: "github.com", Principal: "user:11"},
	})
	require.NoError(err)
	syncer.SetGitHubRouters(map[string]*ghclient.HostRouter{"github.com": router})
	srv := New(database, syncer, nil, "/", nil, ServerOptions{})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	_, err = database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "other", "widget"),
	)
	require.NoError(err)

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/other/widget", nil)
	require.Equal(http.StatusOK, rr.Code)
	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

	assert.False(resp.Operations.MergePR.Available)
	assert.Equal(availabilityCodeMissingWriteCredential, resp.Operations.MergePR.Code)
}

// splitTestDescriptor builds the github.com chain of a split host:
// an installed GitHub App candidate followed by the given user write
// candidate.
func splitTestDescriptor(writeCandidate tokenauth.Candidate) tokenauth.Descriptor {
	return tokenauth.Descriptor{
		Key: tokenauth.Key{Platform: "github", Host: "github.com"},
		Candidates: []tokenauth.Candidate{
			{
				Kind:           tokenauth.SourceKindGitHubApp,
				Host:           "github.com",
				FilePath:       "/keys/app.pem",
				AppID:          7,
				InstallationID: 11,
			},
			writeCandidate,
		},
	}
}

// newSplitTestServer builds a Server whose github.com token chain is
// a split chain (installed app + the given write candidate) so tests
// can exercise mutation availability gating.
func newSplitTestServer(
	t *testing.T, writeCandidate tokenauth.Candidate,
) (*Server, *tokenauth.SourceSet) {
	return newSplitTestServerWithMock(t, writeCandidate, &mockGH{})
}

func newSplitTestServerWithMock(
	t *testing.T, writeCandidate tokenauth.Candidate, mock *mockGH,
) (*Server, *tokenauth.SourceSet) {
	t.Helper()
	database := dbtest.Open(t)
	syncer := ghclient.NewSyncer(
		map[string]ghclient.Client{"github.com": mock},
		database, nil,
		[]ghclient.RepoRef{{Owner: "acme", Name: "widget", PlatformHost: "github.com"}},
		time.Minute, nil, nil,
	)
	t.Cleanup(syncer.Stop)
	set := tokenauth.NewSourceSet(tokenauth.Options{
		GitHubApp: func(context.Context, tokenauth.Candidate) (string, time.Time, error) {
			return "ghs_probe", time.Now().Add(time.Hour), nil
		},
	})
	set.Upsert(splitTestDescriptor(writeCandidate))
	srv := New(database, syncer, nil, "/", nil, ServerOptions{TokenSources: set})
	t.Cleanup(func() { gracefulShutdown(t, srv) })
	_, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(t, err)
	return srv, set
}

func TestAPIRepoResponseIncludesOperationsViewerCannotMerge(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv, database := setupTestServer(t)
	repoID, err := database.UpsertRepo(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	// Schema defaults viewer_can_merge to 1; flip to false so the
	// merge gate (not the capability gate) decides this case.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repoID, false))

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/repo/github/acme/widget", nil)
	require.Equal(http.StatusOK, rr.Code)

	var resp repoResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))

	merge := resp.Operations.MergePR
	assert.False(merge.Available)
	assert.Equal(availabilityCodeViewerCannotMerge, merge.Code)
	assert.Empty(merge.RequiredCapability)

	// Other operations remain available because viewer_can_merge only
	// gates merge_pr.
	assert.True(resp.Operations.ClosePR.Available)
}

func TestAPIPullDetailOperationsDisableSelfApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	mock := &mockGH{
		authenticatedViewerLoginFn: func(context.Context) (string, error) {
			return "marius", nil
		},
	}
	srv, database := setupTestServerWithMock(t, mock)
	seedPR(t, database, "acme", "widget", 1, withSeedPRAuthor("marius"))
	repo, err := database.GetRepoByIdentity(
		t.Context(), verifiedGitHubRepoIdentity("github.com", "acme", "widget"),
	)
	require.NoError(err)
	require.NotNil(repo)
	// Keep merge available so this fixture isolates the self-approval gate.
	require.NoError(database.UpdateRepoViewerCanMerge(t.Context(), repo.ID, true))

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/pulls/github/acme/widget/1", nil)
	require.Equal(http.StatusOK, rr.Code)

	var resp pullapi.MergeRequestDetailResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.NotNil(resp.Repo.Operations)

	submitReview := resp.Repo.Operations.SubmitReview
	assert.False(submitReview.Available)
	assert.Equal(availabilityCodeSelfApproval, submitReview.Code)
	assert.Equal("You cannot approve your own pull request", submitReview.UnavailableReason)
	assert.True(resp.Repo.Operations.MergePR.Available)

	rr = testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/pulls/github/acme/widget/1", nil)
	require.Equal(http.StatusOK, rr.Code)
	assert.Equal(1, mock.authenticatedViewerCalls,
		"provider should cache the authenticated viewer login")
}

func TestAPIPullDetailOperationsSkipViewerLookupWhenSubmitReviewUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	t.Setenv("SPLIT_WRITE_CRED_PAT", "")
	mock := &mockGH{}
	srv, _ := newSplitTestServerWithMock(t, tokenauth.Candidate{
		Kind: tokenauth.SourceKindEnv, EnvName: "SPLIT_WRITE_CRED_PAT",
	}, mock)
	seedPR(t, srv.db, "acme", "widget", 1, withSeedPRAuthor("marius"))

	rr := testutil.DoJSON(t, srv, http.MethodGet, "/api/v1/pulls/github/acme/widget/1", nil)
	require.Equal(http.StatusOK, rr.Code, rr.Body.String())

	var resp pullapi.MergeRequestDetailResponse
	require.NoError(json.NewDecoder(rr.Body).Decode(&resp))
	require.NotNil(resp.Repo.Operations)
	assert.Equal(availabilityCodeMissingWriteCredential, resp.Repo.Operations.SubmitReview.Code)
	assert.Zero(mock.authenticatedViewerCalls,
		"viewer lookup must not run when the write credential already blocks review submission")
}
