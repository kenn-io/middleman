package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	platformgithub "go.kenn.io/forge/platform/github"
)

func TestAdaptPR(t *testing.T) {
	assert := assert.New(t)

	now := time.Now().UTC().Truncate(time.Second)
	merged := now.Add(-time.Hour)

	gql := platformgithub.GraphQLPR{
		DatabaseId:     12345,
		Number:         42,
		Title:          "Fix bug",
		State:          "OPEN",
		IsDraft:        true,
		Locked:         true,
		Body:           "Fixes #1",
		URL:            "https://github.com/o/r/pull/42",
		Additions:      10,
		Deletions:      3,
		Mergeable:      "MERGEABLE",
		ReviewDecision: "APPROVED",
		HeadRefName:    "fix-branch",
		BaseRefName:    "main",
		HeadRefOid:     "abc123",
		BaseRefOid:     "def456",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	gql.Author.Login = "alice"
	gql.MergedAt = &merged
	gql.MergedBy = &struct{ Login string }{Login: "merge-admin"}
	gql.HeadRepository = &struct{ URL string }{URL: "https://github.com/o/r"}

	pr := platformgithub.AdaptPR(&gql)

	assert.Equal(int64(12345), pr.GetID())
	assert.Equal(42, pr.GetNumber())
	assert.Equal("Fix bug", pr.GetTitle())
	assert.Equal("open", pr.GetState())
	assert.True(pr.GetDraft())
	assert.True(pr.GetLocked())
	assert.Equal("Fixes #1", pr.GetBody())
	assert.Equal("https://github.com/o/r/pull/42", pr.GetHTMLURL())
	assert.Equal(10, pr.GetAdditions())
	assert.Equal(3, pr.GetDeletions())
	assert.Equal("alice", pr.GetUser().GetLogin())
	assert.Equal("fix-branch", pr.GetHead().GetRef())
	assert.Equal("main", pr.GetBase().GetRef())
	assert.Equal("abc123", pr.GetHead().GetSHA())
	assert.Equal("def456", pr.GetBase().GetSHA())
	assert.Equal("https://github.com/o/r.git", pr.GetHead().GetRepo().GetCloneURL())
	assert.Equal("clean", pr.GetMergeableState())
	require.NotNil(t, pr.MergedAt)
	assert.True(pr.GetMerged())
	assert.Equal("merge-admin", pr.GetMergedBy().GetLogin())
}

func TestConvertGQLCommentsPreservesMinimizedVisibility(t *testing.T) {
	reason := githubv4.ReportedContentClassifiersOffTopic
	fullDatabaseID := int64(3714845345)
	comment := platformgithub.GraphQLComment{
		DatabaseId:      73,
		FullDatabaseId:  platformgithub.GraphQLInt64(fullDatabaseID),
		IsMinimized:     true,
		MinimizedReason: &reason,
	}

	tests := []struct {
		name    string
		convert func() map[int64]platformgithub.CommentVisibility
	}{
		{
			name: "pull request",
			convert: func() map[int64]platformgithub.CommentVisibility {
				input := platformgithub.GraphQLPR{}
				input.Comments.Nodes = []platformgithub.GraphQLComment{comment}
				return convertGQLPR(&input).CommentVisibility
			},
		},
		{
			name: "issue",
			convert: func() map[int64]platformgithub.CommentVisibility {
				input := platformgithub.GraphQLIssue{}
				input.Comments.Nodes = []platformgithub.GraphQLComment{comment}
				return convertGQLIssue(&input).CommentVisibility
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visibility := tt.convert()
			require.Contains(t, visibility, fullDatabaseID)
			assert.Equal(t, platformgithub.CommentVisibility{Hidden: true, Reason: "OFF_TOPIC"}, visibility[fullDatabaseID])
		})
	}
}

func TestConvertGQLCommentsRecordsObservedVisibleComments(t *testing.T) {
	comment := platformgithub.GraphQLComment{DatabaseId: 74}

	tests := []struct {
		name    string
		convert func() map[int64]platformgithub.CommentVisibility
	}{
		{
			name: "pull request",
			convert: func() map[int64]platformgithub.CommentVisibility {
				input := platformgithub.GraphQLPR{}
				input.Comments.Nodes = []platformgithub.GraphQLComment{comment}
				return convertGQLPR(&input).CommentVisibility
			},
		},
		{
			name: "issue",
			convert: func() map[int64]platformgithub.CommentVisibility {
				input := platformgithub.GraphQLIssue{}
				input.Comments.Nodes = []platformgithub.GraphQLComment{comment}
				return convertGQLIssue(&input).CommentVisibility
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visibility := tt.convert()
			require.Contains(t, visibility, int64(74))
			assert.Equal(t, platformgithub.CommentVisibility{}, visibility[74])
		})
	}
}

func TestConvertGQLPRIncludesReviewThreads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reason := githubv4.ReportedContentClassifiersAbuse
	line := 12
	input := platformgithub.GraphQLPR{}
	threadInput := platformgithub.GraphQLReviewThread{
		ID:         githubv4.ID("PRRT_1"),
		Path:       "src/main.go",
		Line:       line,
		DiffSide:   "RIGHT",
		IsResolved: false,
		IsOutdated: false,
	}
	threadInput.Comments.Nodes = []platformgithub.GraphQLReviewThreadComment{{
		ID:              githubv4.ID("PRRC_1"),
		DatabaseId:      101,
		FullDatabaseId:  3714845345,
		Body:            "hidden inline comment",
		Path:            "src/main.go",
		Line:            line,
		SubjectType:     "LINE",
		IsMinimized:     true,
		MinimizedReason: &reason,
	}}
	input.ReviewThreads.Nodes = []platformgithub.GraphQLReviewThread{threadInput}

	bulk := convertGQLPR(&input)

	require.True(bulk.ReviewThreadsComplete)
	require.Len(bulk.ReviewThreads, 1)
	thread := bulk.ReviewThreads[0]
	assert.Equal("PRRT_1", thread.ProviderThreadID)
	assert.Equal("3714845345", thread.ProviderCommentID)
	assert.Equal("hidden inline comment", thread.Body)
	assert.JSONEq(`{"provider_hidden":true,"provider_hidden_reason":"ABUSE"}`, thread.MetadataJSON)
}

func TestGraphQLFetcherPaginatesCommentVisibility(t *testing.T) {
	tests := []struct {
		name         string
		responseData string
		complete     func(*testing.T, *GraphQLFetcher) map[int64]platformgithub.CommentVisibility
	}{
		{
			name:         "pull request",
			responseData: `{"repository":{"pullRequest":{"comments":{"nodes":[{"databaseId":202,"fullDatabaseId":"3714845345","isMinimized":true,"minimizedReason":"ABUSE"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`,
			complete: func(t *testing.T, fetcher *GraphQLFetcher) map[int64]platformgithub.CommentVisibility {
				pr := platformgithub.GraphQLPR{Number: 7}
				pr.Comments.PageInfo = platformgithub.GraphQLPageInfo{HasNextPage: true, EndCursor: "comment-100"}
				bulk := convertGQLPR(&pr)
				require.NoError(t, fetcher.completePRComments(
					t.Context(), "owner", "repo", &pr, &bulk,
				))
				return bulk.CommentVisibility
			},
		},
		{
			name:         "issue",
			responseData: `{"repository":{"issue":{"comments":{"nodes":[{"databaseId":202,"fullDatabaseId":"3714845345","isMinimized":true,"minimizedReason":"ABUSE"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`,
			complete: func(t *testing.T, fetcher *GraphQLFetcher) map[int64]platformgithub.CommentVisibility {
				issue := platformgithub.GraphQLIssue{Number: 8}
				issue.Comments.PageInfo = platformgithub.GraphQLPageInfo{HasNextPage: true, EndCursor: "comment-100"}
				bulk := convertGQLIssue(&issue)
				require.NoError(t, fetcher.completeIssueCommentVisibility(
					t.Context(), "owner", "repo", &issue, &bulk,
				))
				return bulk.CommentVisibility
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cursor string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Variables map[string]any `json:"variables"`
				}
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				cursor, _ = request.Variables["cursor"].(string)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":%s}`, tt.responseData)
			}))
			defer srv.Close()

			fetcher := NewGraphQLFetcherWithClient(
				githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
			)
			visibility := tt.complete(t, fetcher)

			assert.Equal(t, "comment-100", cursor)
			assert.Equal(t, platformgithub.CommentVisibility{Hidden: true, Reason: "ABUSE"}, visibility[3714845345])
		})
	}
}

func TestGraphQLFetcherFetchesCurrentCommentVisibility(t *testing.T) {
	tests := []struct {
		name     string
		queryKey string
		fetch    func(context.Context, *GraphQLFetcher) (map[int64]platformgithub.CommentVisibility, error)
	}{
		{
			name:     "pull request",
			queryKey: "pullRequest(number:",
			fetch: func(ctx context.Context, fetcher *GraphQLFetcher) (map[int64]platformgithub.CommentVisibility, error) {
				return fetcher.FetchPullRequestCommentVisibility(ctx, "owner", "repo", 7)
			},
		},
		{
			name:     "issue",
			queryKey: "issue(number:",
			fetch: func(ctx context.Context, fetcher *GraphQLFetcher) (map[int64]platformgithub.CommentVisibility, error) {
				return fetcher.FetchIssueCommentVisibility(ctx, "owner", "repo", 8)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				assert.Contains(t, string(body), tt.queryKey)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"data":{"repository":{`+map[string]string{
					"pullRequest(number:": `"pullRequest":{"comments":{"nodes":[{"databaseId":202,"fullDatabaseId":"3714845345","isMinimized":true,"minimizedReason":"ABUSE"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
					"issue(number:":       `"issue":{"comments":{"nodes":[{"databaseId":202,"fullDatabaseId":"3714845345","isMinimized":true,"minimizedReason":"ABUSE"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`,
				}[tt.queryKey]+`}}}`)
			}))
			defer srv.Close()

			fetcher := NewGraphQLFetcherWithClient(
				githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
			)
			visibility, err := tt.fetch(t.Context(), fetcher)
			require.NoError(t, err)
			assert.Equal(t, platformgithub.CommentVisibility{Hidden: true, Reason: "ABUSE"}, visibility[3714845345])
		})
	}
}

func TestAdaptComment(t *testing.T) {
	assert := assert.New(t)
	now := time.Now().UTC().Truncate(time.Second)

	gql := platformgithub.GraphQLComment{
		DatabaseId:     100,
		FullDatabaseId: 3714845345,
		Body:           "LGTM",
		URL:            "https://github.com/acme/widgets/issues/7#issuecomment-100",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	gql.Author.Login = "bob"

	c := adaptComment(&gql)

	assert.Equal(int64(3714845345), c.GetID())
	assert.Equal("LGTM", c.GetBody())
	assert.Equal("https://github.com/acme/widgets/issues/7#issuecomment-100", c.GetHTMLURL())
	assert.Equal("bob", c.GetUser().GetLogin())
}

func TestAdaptReview(t *testing.T) {
	assert := assert.New(t)
	now := time.Now().UTC().Truncate(time.Second)

	gql := platformgithub.GraphQLReview{
		DatabaseId:  200,
		Body:        "Looks good",
		State:       "APPROVED",
		SubmittedAt: now,
	}
	gql.Author.Login = "carol"

	r := adaptReview(&gql)

	assert.Equal(int64(200), r.GetID())
	assert.Equal("Looks good", r.GetBody())
	assert.Equal("APPROVED", r.GetState())
	assert.Equal("carol", r.GetUser().GetLogin())
}

func TestAdaptCommit(t *testing.T) {
	assert := assert.New(t)
	now := time.Now().UTC().Truncate(time.Second)

	gql := platformgithub.GraphQLCommitNode{
		Commit: platformgithub.GraphQLCommit{
			OID:     "sha123",
			Message: "fix: something",
		},
	}
	gql.Commit.Author.Name = "Dave"
	gql.Commit.Author.Date = &now
	gql.Commit.Author.User = &struct{ Login string }{Login: "dave"}
	gql.Commit.Committer.Name = "Eve"
	committedAt := now.Add(time.Hour)
	gql.Commit.Committer.Date = &committedAt
	gql.Commit.Committer.User = &struct{ Login string }{Login: "eve"}

	c := adaptCommit(&gql)

	assert.Equal("sha123", c.GetSHA())
	assert.Equal("fix: something", c.GetCommit().GetMessage())
	assert.Equal("Dave", c.GetCommit().GetAuthor().GetName())
	assert.Equal(now, c.GetCommit().GetAuthor().GetDate().Time)
	assert.Equal("dave", c.GetAuthor().GetLogin())
	assert.Equal("Eve", c.GetCommit().GetCommitter().GetName())
	assert.Equal(committedAt, c.GetCommit().GetCommitter().GetDate().Time)
	assert.Equal("eve", c.GetCommitter().GetLogin())

	gql.Commit.Committer.Date = nil
	withoutCommitterDate := adaptCommit(&gql)
	assert.Nil(withoutCommitterDate.GetCommit().GetCommitter().Date)
}

func TestAdaptCheckContext(t *testing.T) {
	assert := assert.New(t)

	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	contexts := []platformgithub.GraphQLCheckContext{
		{
			Typename: "CheckRun",
			CheckRun: platformgithub.GraphQLCheckRunFields{
				Name:       "ci/test",
				Status:     "COMPLETED",
				Conclusion: "SUCCESS",
				DetailsURL: "https://example.com/1",
			},
		},
		{
			Typename: "StatusContext",
			StatusContext: platformgithub.GraphQLStatusContextFields{
				Context:   "ci/lint",
				State:     "SUCCESS",
				TargetURL: "https://example.com/2",
			},
		},
	}
	contexts[0].CheckRun.CheckSuite.CreatedAt = &now
	contexts[0].CheckRun.CheckSuite.App.Name = "GitHub Actions"

	checks, statuses := splitCheckContexts(contexts)

	require.Len(t, checks, 1)
	assert.Equal("ci/test", checks[0].GetName())
	assert.Equal("completed", checks[0].GetStatus())
	assert.Equal("success", checks[0].GetConclusion())
	assert.Equal("GitHub Actions", checks[0].GetApp().GetName())
	require.NotNil(t, checks[0].GetCheckSuite().CreatedAt)
	assert.True(checks[0].GetCheckSuite().CreatedAt.Time.Equal(now))

	require.Len(t, statuses, 1)
	assert.Equal("ci/lint", statuses[0].GetContext())
	assert.Equal("success", statuses[0].GetState())
}

func TestAdaptCheckRunURLSanitization(t *testing.T) {
	assert := assert.New(t)

	safe := adaptCheckRun(&platformgithub.GraphQLCheckRunFields{
		Name:       "ci",
		Status:     "COMPLETED",
		Conclusion: "SUCCESS",
		DetailsURL: "https://ci.example.com/run/1",
	})
	assert.Equal("https://ci.example.com/run/1", safe.GetHTMLURL())

	unsafe := adaptCheckRun(&platformgithub.GraphQLCheckRunFields{
		Name:       "ci",
		Status:     "COMPLETED",
		Conclusion: "SUCCESS",
		DetailsURL: "javascript:alert(1)",
	})
	assert.Empty(unsafe.GetHTMLURL())
}

func TestGraphqlRateTransport(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	rt := NewRateTracker(d, "github.com", "host", "graphql")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(30*time.Minute).Unix()))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	transport := &graphqlRateTransport{
		base:        http.DefaultTransport,
		rateTracker: rt,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequest("POST", srv.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(1, rt.RequestsThisHour())
	assert.Equal(4999, rt.Remaining())
	assert.Equal(5000, rt.RateLimit())
}

func TestGraphQLFetcherRateTracker(t *testing.T) {
	d := openTestDB(t)
	rt := NewRateTracker(d, "github.com", "host", "graphql")
	f := NewGraphQLFetcher(testTokenSource("fake-token"), "github.com", rt, nil)
	require.Same(t, rt, f.RateTracker())
}

func TestGraphQLFetcherRateTrackerNil(t *testing.T) {
	f := NewGraphQLFetcher(testTokenSource("fake-token"), "github.com", nil, nil)
	require.Nil(t, f.RateTracker())
}

func TestGraphQLFetcherRateTrackerNilReceiver(t *testing.T) {
	var f *GraphQLFetcher
	require.Nil(t, f.RateTracker())
}

func TestConvertGQLPRCompleteness(t *testing.T) {
	assert := assert.New(t)

	gql := platformgithub.GraphQLPR{
		Number:    1,
		Title:     "test",
		State:     "OPEN",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	gql.Author.Login = "user"
	bulk := convertGQLPR(&gql)
	assert.True(bulk.CommentsComplete)
	assert.True(bulk.ReviewsComplete)
	assert.True(bulk.CommitsComplete)
	assert.True(bulk.TimelineComplete)
	assert.True(bulk.CIComplete)

	// Comments incomplete
	gql.Comments.PageInfo.HasNextPage = true
	bulk = convertGQLPR(&gql)
	assert.False(bulk.CommentsComplete)
	assert.True(bulk.ReviewsComplete)

	gql.Comments.PageInfo.HasNextPage = false
	gql.TimelineItems.PageInfo.HasNextPage = true
	bulk = convertGQLPR(&gql)
	assert.False(bulk.TimelineComplete)
}

func TestConvertGQLPRNativeStackHint(t *testing.T) {
	gql := platformgithub.GraphQLPRWithNativeStacks{
		Number: 42,
		Stack: &platformgithub.GraphQLNativeStack{
			Number: 7, Size: 3, BaseRefName: "main",
		},
		StackEntry: &struct{ Position int }{Position: 2},
	}

	bulk := convertGQLPRWithNativeStacks(&gql)
	require.NotNil(t, bulk.NativeStack)
	assert.Equal(t, platformgithub.NativeStackHint{
		Number: 7, Size: 3, Position: 2, BaseRef: "main",
	}, *bulk.NativeStack)
}

func TestGraphQLFetcherOmitsNativeStackFieldsWhenDisabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requestBody []byte
	var requestErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, requestErr = io.ReadAll(r.Body)
		if requestErr != nil {
			http.Error(w, requestErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequests":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`)
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
	)
	_, err := fetcher.FetchRepoPRs(t.Context(), "owner", "repo", false)
	require.NoError(err)
	require.NoError(requestErr)
	assert.NotContains(string(requestBody), "stackEntry")
	assert.NotContains(string(requestBody), "stack{")
}

func TestGraphQLFetcherDropsNativeStackFieldsRejectedBySchema(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "stackEntry") {
			_, _ = io.WriteString(w, `{"errors":[{"message":"Field 'stackEntry' doesn't exist on type 'PullRequest'"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequests":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}`)
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
	)

	first, err := fetcher.FetchRepoPRs(t.Context(), "owner", "repo", true)
	require.NoError(err, "a host without the preview schema must still bulk fetch")
	require.NotNil(first)
	second, err := fetcher.FetchRepoPRs(t.Context(), "owner", "repo", true)
	require.NoError(err)
	require.NotNil(second)

	mu.Lock()
	defer mu.Unlock()
	require.Len(bodies, 3)
	assert.Contains(bodies[0], "stackEntry")
	assert.NotContains(bodies[1], "stackEntry")
	assert.NotContains(bodies[2], "stackEntry",
		"the rejected shape must not be resent on later cycles")
}

func TestGraphQLFetcherFetchRepoPRsIncludesTimelineEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2024, 6, 3, 15, 0, 0, 0, time.UTC).Format(time.RFC3339)

	var sawTimelineItems bool
	var sawNativeStackFields bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sawTimelineItems = bytes.Contains(body, []byte("timelineItems"))
		sawNativeStackFields = bytes.Contains(body, []byte("stackEntry"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequests":{"nodes":[{
			"databaseId":1001,
			"number":1,
			"title":"Timeline PR",
			"state":"OPEN",
			"isDraft":false,
			"body":"",
			"url":"https://github.com/owner/repo/pull/1",
			"author":{"login":"alice"},
			"createdAt":"` + now + `",
			"updatedAt":"` + now + `",
			"mergedAt":null,
			"closedAt":null,
			"additions":1,
			"deletions":0,
			"mergeable":"MERGEABLE",
			"reviewDecision":"",
			"headRefName":"feature",
			"baseRefName":"main",
			"headRefOid":"abc123",
			"baseRefOid":"def456",
			"stack":{"id":"STACK_7","number":7,"size":2,"baseRefName":"main"},
			"stackEntry":{"position":1},
			"headRepository":{"url":"https://github.com/owner/repo"},
			"labels":{"nodes":[]},
			"comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
			"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
			"allCommits":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
			"lastCommit":{"nodes":[]},
			"timelineItems":{"nodes":[{
				"__typename":"HeadRefForcePushedEvent",
				"id":"HFP_1",
				"actor":{"login":"alice"},
				"beforeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"afterCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
				"createdAt":"` + now + `",
				"ref":{"name":"feature"}
			},{
				"__typename":"BaseRefChangedEvent",
				"id":"BRC_1",
				"actor":{"login":"bob"},
				"createdAt":"` + now + `",
				"previousRefName":"main",
				"currentRefName":"release"
			},{
				"__typename":"CommentDeletedEvent",
				"id":"CDE_1",
				"actor":{"login":"maintainer"},
				"createdAt":"` + now + `",
				"deletedCommentAuthor":{"login":"reviewer"}
			},{
				"__typename":"AssignedEvent",
				"id":"AE_1",
				"actor":{"login":"wesm"},
				"assignee":{"__typename":"User","login":"wesm"},
				"createdAt":"` + now + `"
			},{
				"__typename":"MergedEvent",
				"id":"ME_1",
				"actor":{"login":"merger"},
				"createdAt":"` + now + `"
			},{
				"__typename":"ClosedEvent",
				"id":"CE_1",
				"actor":{"login":"closer"},
				"createdAt":"` + now + `"
			},{
				"__typename":"ReopenedEvent",
				"id":"RE_1",
				"actor":{"login":"opener"},
				"createdAt":"` + now + `"
			}],"pageInfo":{"hasNextPage":false,"endCursor":""}}
		}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
	)

	result, err := fetcher.FetchRepoPRs(t.Context(), "owner", "repo", true)
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.PullRequests, 1)
	require.True(sawTimelineItems)
	require.True(sawNativeStackFields)
	require.Len(result.PullRequests[0].TimelineEvents, 7)
	require.NotNil(result.PullRequests[0].NativeStack)
	assert.Equal(7, result.PullRequests[0].NativeStack.Number)

	event := result.PullRequests[0].TimelineEvents[0]
	assert.Equal("force_push", event.EventType)
	assert.Equal("HFP_1", event.NodeID)
	assert.Equal("alice", event.Actor)
	assert.Equal("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", event.BeforeSHA)
	assert.Equal("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", event.AfterSHA)
	assert.Equal("feature", event.Ref)
	event = result.PullRequests[0].TimelineEvents[1]
	assert.Equal("base_ref_changed", event.EventType)
	assert.Equal("BRC_1", event.NodeID)
	assert.Equal("bob", event.Actor)
	assert.Equal("main", event.PreviousRefName)
	assert.Equal("release", event.CurrentRefName)
	deleted := result.PullRequests[0].TimelineEvents[2]
	assert.Equal("comment_deleted", deleted.EventType)
	assert.Equal("CDE_1", deleted.NodeID)
	assert.Equal("maintainer", deleted.Actor)
	assert.Equal("reviewer", deleted.DeletedCommentAuthor)
	assigned := result.PullRequests[0].TimelineEvents[3]
	assert.Equal("assigned", assigned.EventType)
	assert.Equal("AE_1", assigned.NodeID)
	assert.Equal("wesm", assigned.Actor)
	assert.Equal("wesm", assigned.Assignee)
	merged := result.PullRequests[0].TimelineEvents[4]
	assert.Equal("merged", merged.EventType)
	assert.Equal("ME_1", merged.NodeID)
	assert.Equal("merger", merged.Actor)
	closed := result.PullRequests[0].TimelineEvents[5]
	assert.Equal("closed", closed.EventType)
	assert.Equal("CE_1", closed.NodeID)
	assert.Equal("closer", closed.Actor)
	reopened := result.PullRequests[0].TimelineEvents[6]
	assert.Equal("reopened", reopened.EventType)
	assert.Equal("RE_1", reopened.NodeID)
	assert.Equal("opener", reopened.Actor)
	assert.True(result.PullRequests[0].TimelineComplete)
}

func TestGraphQLFetcherFetchRepoIssuesUsesIssueTimelineFragments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2024, 6, 3, 15, 0, 0, 0, time.UTC).Format(time.RFC3339)

	var requestBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		requestBody = body

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{
			"databaseId":2001,
			"number":2,
			"title":"Timeline issue",
			"state":"OPEN",
			"body":"",
			"url":"https://github.com/owner/repo/issues/2",
			"author":{"login":"alice"},
			"createdAt":"` + now + `",
			"updatedAt":"` + now + `",
			"closedAt":null,
			"labels":{"nodes":[]},
			"comments":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
			"timelineItems":{"nodes":[{
				"__typename":"AssignedEvent",
				"id":"AE_issue_1",
				"actor":{"login":"alice"},
				"assignee":{"__typename":"User","login":"bob"},
				"createdAt":"` + now + `"
			},{
				"__typename":"CrossReferencedEvent",
				"id":"CRE_issue_1",
				"actor":{"login":"reviewer"},
				"createdAt":"` + now + `",
				"isCrossRepository":false,
				"willCloseTarget":true,
				"source":{
					"__typename":"PullRequest",
					"number":22,
					"title":"Fix issue",
					"url":"https://github.com/owner/repo/pull/22",
					"repository":{"owner":{"login":"owner"},"name":"repo"}
				}
			},{
				"__typename":"ClosedEvent",
				"id":"CE_issue_1",
				"actor":{"login":"closer"},
				"createdAt":"` + now + `"
			},{
				"__typename":"ReopenedEvent",
				"id":"RE_issue_1",
				"actor":{"login":"opener"},
				"createdAt":"` + now + `"
			}],"pageInfo":{"hasNextPage":false,"endCursor":""}}
		}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
	)

	result, err := fetcher.FetchRepoIssues(t.Context(), "owner", "repo")
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Issues, 1)
	require.Len(result.Issues[0].TimelineEvents, 4)
	assert.Equal("assigned", result.Issues[0].TimelineEvents[0].EventType)
	assert.Equal("bob", result.Issues[0].TimelineEvents[0].Assignee)
	assert.Equal("cross_referenced", result.Issues[0].TimelineEvents[1].EventType)
	assert.Equal(22, result.Issues[0].TimelineEvents[1].SourceNumber)
	assert.Equal("Fix issue", result.Issues[0].TimelineEvents[1].SourceTitle)
	assert.Equal("closed", result.Issues[0].TimelineEvents[2].EventType)
	assert.Equal("closer", result.Issues[0].TimelineEvents[2].Actor)
	assert.Equal("reopened", result.Issues[0].TimelineEvents[3].EventType)
	assert.Equal("opener", result.Issues[0].TimelineEvents[3].Actor)

	assert.Contains(string(requestBody), "AssignedEvent")
	assert.Contains(string(requestBody), "CrossReferencedEvent")
	assert.Contains(string(requestBody), "ClosedEvent")
	assert.Contains(string(requestBody), "ReopenedEvent")
	assert.NotContains(string(requestBody), "HeadRefForcePushedEvent")
	assert.NotContains(string(requestBody), "CommentDeletedEvent")
	assert.NotContains(string(requestBody), "BaseRefChangedEvent")
}

func TestGraphQLFetcherFetchRepoIssuesPreservesBotAuthor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"issues":{"nodes":[{
			"databaseId":2001,
			"number":3,
			"title":"Dependency Dashboard",
			"state":"OPEN",
			"body":"",
			"url":"https://github.com/example/project/issues/3",
			"author":{"__typename":"Bot","login":"renovate"},
			"createdAt":"` + now + `",
			"updatedAt":"` + now + `",
			"closedAt":null,
			"labels":{"nodes":[]},
			"comments":{"totalCount":0,"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}},
			"assignees":{"nodes":[]},
			"timelineItems":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}
		}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`))
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(
		githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil,
	)

	result, err := fetcher.FetchRepoIssues(t.Context(), "example", "project")
	require.NoError(err)
	require.NotNil(result)
	require.Len(result.Issues, 1)
	assert.Equal("renovate[bot]", result.Issues[0].Issue.GetUser().GetLogin())
}

func TestGraphQLFetcherFetchRepoIssuesLogsFetchProgressForPaginatedIssueSet(t *testing.T) {
	require := require.New(t)
	logs := captureDefaultLogs(t)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"repository": map[string]any{"issues": map[string]any{
				"totalCount": 2,
				"nodes":      testGQLIssueNodes(calls),
				"pageInfo": map[string]any{
					"hasNextPage": calls == 1,
					"endCursor":   fmt.Sprintf("cursor-%d", calls),
				},
			}}},
		})
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil)
	result, err := fetcher.FetchRepoIssues(t.Context(), "owner", "repo")
	require.NoError(err)
	require.Len(result.Issues, 2)
	require.Contains(logs.String(), `msg="issue list fetch started"`)
	require.Contains(logs.String(), `msg="issue list fetch completed"`)
	require.Contains(logs.String(), "source=graphql")
}

func testGQLIssueNodes(number int) []map[string]any {
	return []map[string]any{{
		"databaseId": number * 1000,
		"number":     number,
		"title":      fmt.Sprintf("Issue %d", number),
		"state":      "OPEN",
		"url":        fmt.Sprintf("https://github.com/owner/repo/issues/%d", number),
		"author":     map[string]any{"login": "alice"},
		"createdAt":  "2026-05-20T12:00:00Z",
		"updatedAt":  "2026-05-20T12:00:00Z",
	}}
}

func TestGraphQLFetcherFetchRepoPRsLogsFetchProgressForPaginatedPullRequestSet(t *testing.T) {
	require := require.New(t)
	logs := captureDefaultLogs(t)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
				"totalCount": 2,
				"nodes":      testGQLPRNodes(calls),
				"pageInfo": map[string]any{
					"hasNextPage": calls == 1,
					"endCursor":   fmt.Sprintf("cursor-%d", calls),
				},
			}}},
		})
	}))
	defer srv.Close()

	fetcher := NewGraphQLFetcherWithClient(githubv4.NewEnterpriseClient(srv.URL, srv.Client()), nil)
	result, err := fetcher.FetchRepoPRs(t.Context(), "conda-forge", "staged-recipes", false)
	require.NoError(err)
	require.Len(result.PullRequests, 2)
	require.Contains(logs.String(), `msg="merge request list fetch started"`)
	require.Contains(logs.String(), `msg="merge request list fetch completed"`)
	require.Contains(logs.String(), "source=graphql")
}

func testGQLPRNodes(number int) []map[string]any {
	return []map[string]any{{
		"databaseId":  number * 1000,
		"number":      number,
		"title":       fmt.Sprintf("Pull request %d", number),
		"state":       "OPEN",
		"url":         fmt.Sprintf("https://github.com/conda-forge/staged-recipes/pull/%d", number),
		"author":      map[string]any{"login": "alice"},
		"createdAt":   "2026-05-20T12:00:00Z",
		"updatedAt":   "2026-05-20T12:00:00Z",
		"headRefName": "recipe",
		"baseRefName": "main",
		"headRefOid":  "abc123",
		"baseRefOid":  "def456",
		"headRepository": map[string]any{
			"url": "https://github.com/conda-forge/staged-recipes",
		},
	}}
}

func TestNormalizeBulkCI(t *testing.T) {
	assert := assert.New(t)

	nameTest := "test"
	statusCompleted := "completed"
	conclusionSuccess := "success"
	detailsURL := "https://example.com"
	appName := "Actions"
	contextLint := "ci/lint"
	stateSuccess := "success"
	targetURL := "https://example.com/2"

	bulk := &BulkPR{
		CheckRuns: []*gh.CheckRun{
			{
				Name:       &nameTest,
				Status:     &statusCompleted,
				Conclusion: &conclusionSuccess,
				DetailsURL: &detailsURL,
				App:        &gh.App{Name: &appName},
			},
		},
		Statuses: []*gh.RepoStatus{
			{
				Context:   &contextLint,
				State:     &stateSuccess,
				TargetURL: &targetURL,
			},
		},
	}

	checks := normalizeBulkCI(bulk)
	require.Len(t, checks, 2)
	assert.Equal("ci/lint", checks[0].Name)
	assert.Equal("completed", checks[0].Status)
	assert.Equal("test", checks[1].Name)
	assert.Equal("completed", checks[1].Status)
}

func TestAdaptPRNilFields(t *testing.T) {
	assert := assert.New(t)

	gql := platformgithub.GraphQLPR{
		Number:    1,
		Title:     "test",
		State:     "OPEN",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	// HeadRepository is nil
	pr := platformgithub.AdaptPR(&gql)
	assert.Nil(pr.GetHead().GetRepo())
	assert.Nil(pr.MergedAt)
	assert.False(pr.GetMerged())
}

func TestAdaptIssue(t *testing.T) {
	assert := assert.New(t)

	now := time.Now().UTC().Truncate(time.Second)
	closed := now.Add(-time.Hour)

	gql := platformgithub.GraphQLIssue{
		DatabaseId: 99999,
		Number:     10,
		Title:      "Bug report",
		State:      "OPEN",
		Body:       "Something broke",
		URL:        "https://github.com/o/r/issues/10",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	gql.Author.Login = "alice"
	gql.Labels.Nodes = []platformgithub.GraphQLLabel{
		{Name: "bug", Color: "d73a4a", Description: "Something broken", IsDefault: false},
	}
	gql.Comments.Nodes = []platformgithub.GraphQLComment{
		{DatabaseId: 501, Body: "I see this too", CreatedAt: now, UpdatedAt: now},
	}
	gql.Comments.Nodes[0].Author.Login = "bob"

	issue := adaptIssue(&gql)

	assert.Equal(int64(99999), issue.GetID())
	assert.Equal(10, issue.GetNumber())
	assert.Equal("Bug report", issue.GetTitle())
	assert.Equal("open", issue.GetState())
	assert.Equal("Something broke", issue.GetBody())
	assert.Equal("https://github.com/o/r/issues/10", issue.GetHTMLURL())
	assert.Equal("alice", issue.GetUser().GetLogin())
	require.Len(t, issue.Labels, 1)
	assert.Equal("bug", issue.Labels[0].GetName())
	assert.Equal("d73a4a", issue.Labels[0].GetColor())
	assert.Nil(issue.ClosedAt)
	// Comments.TotalCount should map to issue comment count, not len(Nodes).
	assert.Equal(0, issue.GetComments())

	// TotalCount > len(Nodes): server has more comments than page returned.
	gql.Comments.TotalCount = 42
	issue = adaptIssue(&gql)
	assert.Equal(42, issue.GetComments())

	// Test closed state
	gql.State = "CLOSED"
	gql.ClosedAt = &closed
	issue = adaptIssue(&gql)
	assert.Equal("closed", issue.GetState())
	require.NotNil(t, issue.ClosedAt)
	assert.Equal(closed, issue.ClosedAt.Time)
}

func TestAdaptIssueNilFields(t *testing.T) {
	assert := assert.New(t)

	gql := platformgithub.GraphQLIssue{
		Number:    1,
		Title:     "minimal",
		State:     "OPEN",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	// Author empty, no labels, no ClosedAt
	issue := adaptIssue(&gql)
	assert.Empty(issue.GetUser().GetLogin())
	assert.Nil(issue.ClosedAt)
	assert.Empty(issue.Labels)
}

// TestAdaptIssueAssignees verifies the GraphQL bulk path populates
// gh.Issue.Assignees from gqlIssue.Assignees.Nodes. Without this, large
// repos using bulk GraphQL sync would persist assignees as [] and overwrite
// any values set by a prior REST detail fetch (roborev finding on 2b9ca4d).
func TestAdaptIssueAssignees(t *testing.T) {
	assert := assert.New(t)

	gql := platformgithub.GraphQLIssue{
		Number:    2,
		Title:     "Assigned issue",
		State:     "OPEN",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	gql.Assignees.Nodes = []platformgithub.GraphQLAssigneeID{
		{Login: "alice"},
		{Login: "bob"},
	}

	issue := adaptIssue(&gql)
	require.Len(t, issue.Assignees, 2)
	assert.Equal("alice", issue.Assignees[0].GetLogin())
	assert.Equal("bob", issue.Assignees[1].GetLogin())
}

func TestConvertGQLIssue(t *testing.T) {
	assert := assert.New(t)

	now := time.Now()
	gql := platformgithub.GraphQLIssue{
		DatabaseId: 1,
		Number:     5,
		Title:      "test",
		State:      "OPEN",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	gql.Author.Login = "user"

	// All complete (no next page)
	bulk := convertGQLIssue(&gql)
	assert.True(bulk.CommentsComplete)
	assert.True(bulk.TimelineComplete)
	assert.NotNil(bulk.Issue)
	assert.Equal(5, bulk.Issue.GetNumber())
	assert.Empty(bulk.Comments)

	// Add comments with next page
	gql.Comments.Nodes = []platformgithub.GraphQLComment{
		{DatabaseId: 100, Body: "hello", CreatedAt: now, UpdatedAt: now},
	}
	gql.Comments.Nodes[0].Author.Login = "commenter"
	gql.Comments.PageInfo.HasNextPage = true

	bulk = convertGQLIssue(&gql)
	assert.False(bulk.CommentsComplete)
	require.Len(t, bulk.Comments, 1)
	assert.Equal("hello", bulk.Comments[0].GetBody())

	gql.Comments.PageInfo.HasNextPage = false
	gql.TimelineItems.Nodes = []platformgithub.GraphQLIssueTimelineItem{{
		Typename: "AssignedEvent",
		Node:     platformgithub.GraphQLNodeFragment{ID: "AE_1"},
		AssignedEvent: platformgithub.GraphQLAssignedEvent{
			Actor:     &platformgithub.GraphQLActorRef{Login: "wesm"},
			Assignee:  platformgithub.GraphQLAssignee{Typename: "User", User: platformgithub.GraphQLAssigneeID{Login: "wesm"}},
			CreatedAt: now,
		},
	}}
	gql.TimelineItems.PageInfo.HasNextPage = true

	bulk = convertGQLIssue(&gql)
	assert.False(bulk.TimelineComplete)
	require.Len(t, bulk.TimelineEvents, 1)
	assert.Equal("assigned", bulk.TimelineEvents[0].EventType)
	assert.Equal("wesm", bulk.TimelineEvents[0].Assignee)
}

func TestStateConversion(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("open", platformgithub.StateToREST("OPEN"))
	assert.Equal("closed", platformgithub.StateToREST("CLOSED"))
	assert.Equal("closed", platformgithub.StateToREST("MERGED"))
}

func TestMergeableConversion(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("clean", platformgithub.MergeableToREST("MERGEABLE"))
	assert.Equal("dirty", platformgithub.MergeableToREST("CONFLICTING"))
	assert.Equal("unknown", platformgithub.MergeableToREST("UNKNOWN"))
}

func TestNormalizeBulkCIPendingStatus(t *testing.T) {
	assert := assert.New(t)

	contextDeploy := "ci/deploy"
	statePending := "pending"
	pendingURL := "https://example.com"

	bulk := &BulkPR{
		Statuses: []*gh.RepoStatus{
			{
				Context:   &contextDeploy,
				State:     &statePending,
				TargetURL: &pendingURL,
			},
		},
	}

	checks := normalizeBulkCI(bulk)
	require.Len(t, checks, 1)
	assert.Equal("ci/deploy", checks[0].Name)
	assert.Equal("in_progress", checks[0].Status)
	assert.Empty(checks[0].Conclusion)
}

func TestNormalizeBulkCI_SortsByCasefoldedName(t *testing.T) {
	assert := assert.New(t)

	buildName := "build"
	zebraName := "Zebra"
	alphaContext := "alpha"
	statusCompleted := "completed"
	conclusionSuccess := "success"
	stateSuccess := "success"

	checks := normalizeBulkCI(&BulkPR{
		CheckRuns: []*gh.CheckRun{
			{Name: &buildName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
			{Name: &zebraName, Status: &statusCompleted, Conclusion: &conclusionSuccess},
		},
		Statuses: []*gh.RepoStatus{
			{Context: &alphaContext, State: &stateSuccess},
		},
	})
	require.Len(t, checks, 3)

	assert.Equal("alpha", checks[0].Name)
	assert.Equal("build", checks[1].Name)
	assert.Equal("Zebra", checks[2].Name)
}

func TestNormalizeBulkCI_LatestCheckRunPerNameWins(t *testing.T) {
	assert := assert.New(t)

	older := gh.Timestamp{Time: time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)}
	newer := gh.Timestamp{Time: older.Add(10 * time.Minute)}
	buildName := "build"
	statusCompleted := "completed"
	conclusionFailure := "failure"
	conclusionSuccess := "success"

	checks := normalizeBulkCI(&BulkPR{
		CheckRuns: []*gh.CheckRun{
			{
				ID:          new(int64(100)),
				Name:        &buildName,
				Status:      &statusCompleted,
				Conclusion:  &conclusionFailure,
				CompletedAt: &older,
			},
			{
				ID:          new(int64(101)),
				Name:        &buildName,
				Status:      &statusCompleted,
				Conclusion:  &conclusionSuccess,
				CompletedAt: &newer,
			},
		},
	})

	require.Len(t, checks, 1)
	assert.Equal("build", checks[0].Name)
	assert.Equal("success", checks[0].Conclusion)
}
