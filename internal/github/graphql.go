package github

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v89/github"
	"github.com/shurcooL/githubv4"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

func gqlCommentVisibility(comment *platformgithub.GraphQLComment) platformgithub.CommentVisibility {
	return commentVisibility(comment.IsMinimized, comment.MinimizedReason)
}

// topLevelPageSize is the number of PRs fetched per GraphQL
// query page. Kept conservative to stay under GitHub's 500k
// node limit even with nested connections.
const topLevelPageSize = 10

// retryPageSize is used when the initial query fails (e.g.,
// complexity/node limit error). Half the default.
const retryPageSize = 5

// --- GraphQL query types (private) ---

type gqlPRQuery[T any] struct {
	Repository struct {
		PullRequests struct {
			TotalCount int
			Nodes      []T
			PageInfo   platformgithub.GraphQLPageInfo
		} `graphql:"pullRequests(first: $pageSize, states: OPEN, after: $cursor)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type gqlIssueQuery struct {
	Repository struct {
		Issues struct {
			TotalCount int
			Nodes      []platformgithub.GraphQLIssue
			PageInfo   platformgithub.GraphQLPageInfo
		} `graphql:"issues(first: $pageSize, states: OPEN, after: $cursor)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type gqlPRCommentPageQuery struct {
	Repository struct {
		PullRequest *struct {
			Comments struct {
				Nodes    []platformgithub.GraphQLComment
				PageInfo platformgithub.GraphQLPageInfo
			} `graphql:"comments(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type gqlPRCommentVisibilityPageQuery struct {
	Repository struct {
		PullRequest *struct {
			Comments struct {
				Nodes    []platformgithub.GraphQLCommentVisibilityNode
				PageInfo platformgithub.GraphQLPageInfo
			} `graphql:"comments(first: 100, after: $cursor)"`
		} `graphql:"pullRequest(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

type gqlIssueCommentPageQuery struct {
	Repository struct {
		Issue *struct {
			Comments struct {
				Nodes    []platformgithub.GraphQLCommentVisibilityNode
				PageInfo platformgithub.GraphQLPageInfo
			} `graphql:"comments(first: 100, after: $cursor)"`
		} `graphql:"issue(number: $number)"`
	} `graphql:"repository(owner: $owner, name: $name)"`
}

// --- Adapter functions ---

func adaptIssue(gql *platformgithub.GraphQLIssue) *gh.Issue {
	state := platformgithub.StateToREST(gql.State)
	authorLogin := gql.Author.Login
	if gql.Author.Typename == "Bot" && !strings.HasSuffix(authorLogin, "[bot]") {
		authorLogin += "[bot]"
	}
	issue := &gh.Issue{
		ID:       new(gql.DatabaseId),
		Number:   new(gql.Number),
		Title:    new(gql.Title),
		State:    new(state),
		Body:     new(gql.Body),
		HTMLURL:  new(gql.URL),
		Comments: new(gql.Comments.TotalCount),
		User:     &gh.User{Login: new(authorLogin)},
	}

	created := gh.Timestamp{Time: gql.CreatedAt}
	updated := gh.Timestamp{Time: gql.UpdatedAt}
	issue.CreatedAt = &created
	issue.UpdatedAt = &updated

	if gql.ClosedAt != nil {
		t := gh.Timestamp{Time: *gql.ClosedAt}
		issue.ClosedAt = &t
	}
	issue.Labels = platformgithub.AdaptLabels(gql.Labels.Nodes)
	issue.Assignees = platformgithub.AdaptAssignees(gql.Assignees.Nodes)

	return issue
}

func adaptComment(gql *platformgithub.GraphQLComment) *gh.IssueComment {
	created := gh.Timestamp{Time: gql.CreatedAt}
	updated := gh.Timestamp{Time: gql.UpdatedAt}
	return &gh.IssueComment{
		ID:        new(platformgithub.FirstPositiveInt64(int64(gql.FullDatabaseId), gql.DatabaseId)),
		Body:      new(gql.Body),
		HTMLURL:   new(gql.URL),
		User:      &gh.User{Login: new(gql.Author.Login)},
		CreatedAt: &created,
		UpdatedAt: &updated,
	}
}

func adaptReview(gql *platformgithub.GraphQLReview) *gh.PullRequestReview {
	submitted := gh.Timestamp{Time: gql.SubmittedAt}
	return &gh.PullRequestReview{
		ID:          new(gql.DatabaseId),
		Body:        new(gql.Body),
		State:       new(gql.State),
		User:        &gh.User{Login: new(gql.Author.Login)},
		SubmittedAt: &submitted,
	}
}

func adaptCommit(gql *platformgithub.GraphQLCommitNode) *gh.RepositoryCommit {
	c := &gh.RepositoryCommit{
		SHA: new(gql.Commit.OID),
		Commit: &gh.Commit{
			Message: new(gql.Commit.Message),
			Author: &gh.CommitAuthor{
				Name: new(gql.Commit.Author.Name),
				Date: ghTimestampPtr(gql.Commit.Author.Date),
			},
			Committer: &gh.CommitAuthor{
				Name: new(gql.Commit.Committer.Name),
				Date: ghTimestampPtr(gql.Commit.Committer.Date),
			},
		},
	}
	if gql.Commit.Author.User != nil {
		c.Author = &gh.User{Login: new(gql.Commit.Author.User.Login)}
	}
	if gql.Commit.Committer.User != nil {
		c.Committer = &gh.User{Login: new(gql.Commit.Committer.User.Login)}
	}
	return c
}

func splitCheckContexts(contexts []platformgithub.GraphQLCheckContext) ([]*gh.CheckRun, []*gh.RepoStatus) {
	var checks []*gh.CheckRun
	var statuses []*gh.RepoStatus
	for i := range contexts {
		c := &contexts[i]
		switch c.Typename {
		case "CheckRun":
			checks = append(checks, adaptCheckRun(&c.CheckRun))
		case "StatusContext":
			statuses = append(statuses, adaptStatusContext(&c.StatusContext))
		}
	}
	return checks, statuses
}

func adaptCheckRun(gql *platformgithub.GraphQLCheckRunFields) *gh.CheckRun {
	url := sanitizeURL(gql.DetailsURL)
	return &gh.CheckRun{
		Name:        new(gql.Name),
		Status:      new(toLower(gql.Status)),
		Conclusion:  new(toLower(gql.Conclusion)),
		HTMLURL:     new(url),
		DetailsURL:  new(gql.DetailsURL),
		StartedAt:   ghTimestampPtr(gql.StartedAt),
		CompletedAt: ghTimestampPtr(gql.CompletedAt),
		CheckSuite:  &gh.CheckSuite{CreatedAt: ghTimestampPtr(gql.CheckSuite.CreatedAt)},
		App:         &gh.App{Name: new(gql.CheckSuite.App.Name)},
	}
}

func adaptStatusContext(gql *platformgithub.GraphQLStatusContextFields) *gh.RepoStatus {
	return &gh.RepoStatus{
		Context:   new(gql.Context),
		State:     new(toLower(gql.State)),
		TargetURL: new(gql.TargetURL),
	}
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func ghTimestampPtr(t *time.Time) *gh.Timestamp {
	if t == nil {
		return nil
	}
	return &gh.Timestamp{Time: *t}
}

// --- Bulk result types ---

// RepoBulkResult holds all open PRs and issues fetched via GraphQL for a repo.
type RepoBulkResult struct {
	PullRequests []BulkPR
	Issues       []BulkIssue
	// NativeStacksQueried reports whether the query that produced this result
	// asked for the preview stack fields. When it did not, the absent hints say
	// nothing about stack membership and must not replace hints from another
	// source.
	NativeStacksQueried bool
}

// BulkIssue holds an issue and its nested comments from a single
// GraphQL query. CommentsComplete indicates whether the comments
// connection was fully paginated.
type BulkIssue struct {
	Issue             *gh.Issue
	Comments          []*gh.IssueComment
	CommentVisibility map[int64]platformgithub.CommentVisibility
	TimelineEvents    []platformgithub.PullRequestTimelineEvent
	CommentsComplete  bool
	TimelineComplete  bool
}

// BulkPR holds a PR and its nested data from a single GraphQL query.
// The *Complete flags indicate whether each nested connection was
// fully paginated. When false, the data is partial and the detail
// drain should fill in via REST.
type BulkPR struct {
	PR *gh.PullRequest
	// NativeStack is present only when the preview setting was enabled and
	// GitHub reported authoritative membership for this PR.
	NativeStack *platformgithub.NativeStackHint
	// ReviewDecision is GitHub's authoritative aggregate review decision for
	// the PR (raw GraphQL enum: APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED,
	// or empty when the repository enforces no decision). It is computed by the
	// provider over the PR's full review history, so it does not depend on the
	// Reviews connection being complete.
	ReviewDecision        string
	Comments              []*gh.IssueComment
	CommentVisibility     map[int64]platformgithub.CommentVisibility
	Reviews               []*gh.PullRequestReview
	ReviewThreads         []platform.MergeRequestReviewThread
	Commits               []*gh.RepositoryCommit
	TimelineEvents        []platformgithub.PullRequestTimelineEvent
	CheckRuns             []*gh.CheckRun
	Statuses              []*gh.RepoStatus
	CommentsComplete      bool
	ReviewsComplete       bool
	ReviewThreadsComplete bool
	CommitsComplete       bool
	TimelineComplete      bool
	CIComplete            bool
}

func platformReviewThreadsFromGQL(threads []platformgithub.GraphQLReviewThread) []platform.MergeRequestReviewThread {
	var out []platform.MergeRequestReviewThread
	for i := range threads {
		thread := &threads[i]
		normalizedThread := platformgithub.PullRequestReviewThread{
			NodeID:            fmt.Sprint(thread.ID),
			IsResolved:        thread.IsResolved,
			IsOutdated:        thread.IsOutdated,
			Path:              thread.Path,
			Side:              thread.DiffSide,
			StartLine:         thread.StartLine,
			OriginalStartLine: thread.OriginalStartLine,
			Line:              thread.Line,
			OriginalLine:      thread.OriginalLine,
		}
		for j := range thread.Comments.Nodes {
			comment := &thread.Comments.Nodes[j]
			reason := ""
			if comment.MinimizedReason != nil {
				reason = string(*comment.MinimizedReason)
			}
			normalizedComment := platformgithub.PullRequestReviewThreadComment{
				NodeID:          fmt.Sprint(comment.ID),
				DatabaseID:      platformgithub.FirstPositiveInt64(int64(comment.FullDatabaseId), comment.DatabaseId),
				SubjectType:     comment.SubjectType,
				Body:            comment.Body,
				AuthorLogin:     comment.Author.Login,
				Path:            comment.Path,
				Line:            comment.Line,
				OriginalLine:    comment.OriginalLine,
				DiffHunk:        comment.DiffHunk,
				URL:             comment.URL,
				IsMinimized:     comment.IsMinimized,
				MinimizedReason: reason,
				CreatedAt:       comment.CreatedAt,
				UpdatedAt:       comment.UpdatedAt,
			}
			if comment.Commit != nil {
				normalizedComment.CommitID = comment.Commit.OID
			}
			if comment.OriginalCommit != nil {
				normalizedComment.OriginalCommitID = comment.OriginalCommit.OID
			}
			if comment.PullRequestReview != nil {
				normalizedComment.ReviewDatabaseID = comment.PullRequestReview.DatabaseId
			}
			out = append(out, platformgithub.GithubReviewThreadComment(normalizedThread, normalizedComment))
		}
	}
	return out
}

func commentVisibility(
	isMinimized bool,
	minimizedReason *githubv4.ReportedContentClassifiers,
) platformgithub.CommentVisibility {
	if !isMinimized {
		return platformgithub.CommentVisibility{}
	}
	reason := ""
	if minimizedReason != nil {
		reason = string(*minimizedReason)
	}
	return platformgithub.CommentVisibility{Hidden: true, Reason: reason}
}

func convertGQLIssue(gql *platformgithub.GraphQLIssue) BulkIssue {
	bulk := BulkIssue{
		Issue:             adaptIssue(gql),
		CommentVisibility: make(map[int64]platformgithub.CommentVisibility),
		CommentsComplete:  !gql.Comments.PageInfo.HasNextPage,
		TimelineComplete:  !gql.TimelineItems.PageInfo.HasNextPage,
	}

	for i := range gql.Comments.Nodes {
		comment := &gql.Comments.Nodes[i]
		bulk.Comments = append(bulk.Comments, adaptComment(comment))
		bulk.CommentVisibility[platformgithub.FirstPositiveInt64(
			int64(comment.FullDatabaseId), comment.DatabaseId,
		)] = gqlCommentVisibility(comment)
	}
	for i := range gql.TimelineItems.Nodes {
		event, ok := adaptIssueTimelineEvent(&gql.TimelineItems.Nodes[i])
		if ok {
			bulk.TimelineEvents = append(bulk.TimelineEvents, event)
		}
	}

	return bulk
}

// --- GraphQL rate transport ---

type graphqlRateTransport struct {
	base        http.RoundTripper
	rateTracker *RateTracker
}

func (t *graphqlRateTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if t.rateTracker != nil {
		t.rateTracker.RecordRequest()
		if rate := parseRateLimitHeaders(resp); rate.Limit > 0 {
			t.rateTracker.UpdateFromRate(rate)
		}
	}
	return resp, err
}

func parseRateLimitHeaders(resp *http.Response) Rate {
	var rate Rate
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		rate.Remaining, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		rate.Limit, _ = strconv.Atoi(v)
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		epoch, _ := strconv.ParseInt(v, 10, 64)
		rate.Reset = time.Unix(epoch, 0)
	}
	return rate
}

// --- GraphQLFetcher ---

// GraphQLFetcher fetches PR data via GitHub's GraphQL API (v4).
type GraphQLFetcher struct {
	client        *githubv4.Client
	rateTracker   *RateTracker
	quotaRegistry *QuotaRegistry
	readIdentity  IdentityKey
	host          string
	// nativeStacksRejected latches when this host validates the preview-only
	// stack fields as unknown. Hosts without the preview reject the shape on
	// every query, so re-sending it would abandon bulk fetch each cycle. A
	// process restart is the reset point once the host gains the preview.
	nativeStacksRejected atomic.Bool
}

type GraphQLFetcherOption func(*graphQLFetcherOptions)

type graphQLFetcherOptions struct {
	quotaRegistry *QuotaRegistry
	readIdentity  IdentityKey
}

// WithGraphQLQuotaAccounting records the fetcher's GraphQL quota against the
// principal authenticating its reads. A fetcher only ever reads, so it needs
// no write identity.
func WithGraphQLQuotaAccounting(
	registry *QuotaRegistry, readIdentity IdentityKey,
) GraphQLFetcherOption {
	return func(options *graphQLFetcherOptions) {
		options.quotaRegistry = registry
		options.readIdentity = readIdentity
	}
}

// RateTracker returns the GraphQL rate tracker, or nil if none
// (or if called on a nil receiver).
func (f *GraphQLFetcher) RateTracker() *RateTracker {
	if f == nil {
		return nil
	}
	return f.rateTracker
}

func (f *GraphQLFetcher) QuotaRegistry() *QuotaRegistry {
	if f == nil {
		return nil
	}
	return f.quotaRegistry
}

// ReadIdentity returns the principal whose GraphQL pool this fetcher spends.
func (f *GraphQLFetcher) ReadIdentity() IdentityKey {
	if f == nil {
		return IdentityKey{}
	}
	return f.readIdentity
}

// NewGraphQLFetcher creates a fetcher for the given host. budget may be nil.
func NewGraphQLFetcher(
	source tokenauth.Source,
	platformHost string,
	rateTracker *RateTracker,
	budget *SyncBudget,
	options ...GraphQLFetcherOption,
) *GraphQLFetcher {
	var resolvedOptions graphQLFetcherOptions
	for _, option := range options {
		option(&resolvedOptions)
	}
	// The budget transport sits beneath AuthTransport so every wire
	// attempt authRT makes on a request — including its own internal
	// 401-invalidate-retry — is counted as a separate spend, matching the
	// REST client's layering (internal/github/client.go). Layering it
	// above authRT would let a 401-then-retry count as a single spend
	// since AuthTransport's retry never becomes visible to a wrapper
	// above it.
	var readBase = WrapSyncBudgetTransport(
		http.DefaultTransport, budget,
	)
	if resolvedOptions.quotaRegistry != nil &&
		resolvedOptions.readIdentity.Principal != "" {
		readBase = &quotaTransport{
			base: readBase, registry: resolvedOptions.quotaRegistry,
			identity: resolvedOptions.readIdentity, resource: QuotaResourceGraphQL,
		}
	}
	authRT := platform.AuthTransport{
		Source:              source,
		Base:                readBase,
		SetHeader:           platform.BearerAuthHeader,
		RetryOnUnauthorized: true,
		AllowedOrigin:       graphQLEndpointForHost(platformHost),
		TokenContext:        githubCredentialContext,
	}
	var base http.RoundTripper = authRT
	if rateTracker != nil {
		base = &graphqlRateTransport{
			base:        base,
			rateTracker: rateTracker,
		}
	}
	httpClient := &http.Client{Transport: wrapPublicGitHubAPIGuard(base)}

	var gqlClient *githubv4.Client
	if platformHost == "" || platformHost == "github.com" {
		gqlClient = githubv4.NewClient(httpClient)
	} else {
		endpoint := graphQLEndpointForHost(platformHost)
		gqlClient = githubv4.NewEnterpriseClient(endpoint, httpClient)
	}

	return &GraphQLFetcher{
		client:        gqlClient,
		rateTracker:   rateTracker,
		quotaRegistry: resolvedOptions.quotaRegistry,
		readIdentity:  resolvedOptions.readIdentity,
		host:          platformHost,
	}
}

// NewGraphQLFetcherWithClient wraps a pre-built githubv4.Client as a
// GraphQLFetcher. Used by tests that need to point the fetcher at a
// mock HTTP backend.
func NewGraphQLFetcherWithClient(
	client *githubv4.Client, rateTracker *RateTracker,
) *GraphQLFetcher {
	return &GraphQLFetcher{
		client:      client,
		rateTracker: rateTracker,
	}
}

func (g *GraphQLFetcher) ShouldBackoff() (bool, time.Duration) {
	if g.rateTracker == nil {
		return false, 0
	}
	return g.rateTracker.ShouldBackoff()
}

func (g *GraphQLFetcher) FetchRepoPRs(
	ctx context.Context, owner, name string, includeNativeStacks bool,
) (*RepoBulkResult, error) {
	includeNativeStacks = includeNativeStacks && !g.nativeStacksRejected.Load()
	result, err := g.fetchRepoPRsWithPageSize(
		ctx, owner, name, topLevelPageSize, includeNativeStacks,
	)
	if err != nil && includeNativeStacks && isNativeStackSchemaRejection(err) {
		// Retrying the same shape cannot help: the host's schema has no
		// preview stack fields. Drop them and keep bulk fetch instead of
		// degrading every cycle to the REST index. REST-derived stack hints
		// still reach the native cache refresh.
		slog.Warn("GraphQL rejected native stack preview fields, querying without them",
			"owner", owner, "name", name, "err", err,
		)
		g.nativeStacksRejected.Store(true)
		includeNativeStacks = false
		result, err = g.fetchRepoPRsWithPageSize(
			ctx, owner, name, topLevelPageSize, false,
		)
	}
	if err != nil {
		slog.Warn("GraphQL query failed, retrying with smaller page",
			"owner", owner, "name", name,
			"err", err, "retryPageSize", retryPageSize,
		)
		result, err = g.fetchRepoPRsWithPageSize(
			ctx, owner, name, retryPageSize, includeNativeStacks,
		)
	}
	return result, err
}

// isNativeStackSchemaRejection reports whether err is GraphQL schema validation
// rejecting the preview-only stack fields rather than a transient or
// complexity-related failure.
func isNativeStackSchemaRejection(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "stack") {
		return false
	}
	for _, phrase := range []string{
		"doesn't exist on type",
		"does not exist on type",
		"cannot query field",
		"unknown field",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func (g *GraphQLFetcher) fetchRepoPRsWithPageSize(
	ctx context.Context, owner, name string, pageSize int, includeNativeStacks bool,
) (*RepoBulkResult, error) {
	ctx = tokenauth.WithGitHubOwner(ctx, owner)
	progress := newMergeRequestListFetchProgressLogger(RepoRef{
		Owner:        owner,
		Name:         name,
		PlatformHost: g.host,
	}, "graphql")
	result := &RepoBulkResult{NativeStacksQueried: includeNativeStacks}
	if includeNativeStacks {
		gqlPRs, err := fetchGraphQLPullRequestPages[platformgithub.GraphQLPRWithNativeStacks](
			ctx, g.client, owner, name, pageSize, progress,
		)
		if err != nil {
			return nil, err
		}
		result.PullRequests = make([]BulkPR, 0, len(gqlPRs))
		for i := range gqlPRs {
			bulk := convertGQLPRWithNativeStacks(&gqlPRs[i])
			if err := g.completePRComments(ctx, owner, name, &gqlPRs[i].GraphQLPR, &bulk); err != nil {
				return nil, err
			}
			result.PullRequests = append(result.PullRequests, bulk)
		}
	} else {
		gqlPRs, err := fetchGraphQLPullRequestPages[platformgithub.GraphQLPR](
			ctx, g.client, owner, name, pageSize, progress,
		)
		if err != nil {
			return nil, err
		}
		result.PullRequests = make([]BulkPR, 0, len(gqlPRs))
		for i := range gqlPRs {
			bulk := convertGQLPR(&gqlPRs[i])
			if err := g.completePRComments(ctx, owner, name, &gqlPRs[i], &bulk); err != nil {
				return nil, err
			}
			result.PullRequests = append(result.PullRequests, bulk)
		}
	}
	progress.done()
	return result, nil
}

func fetchGraphQLPullRequestPages[T any](
	ctx context.Context,
	client *githubv4.Client,
	owner, name string,
	pageSize int,
	progress *listFetchProgressLogger,
) ([]T, error) {
	return fetchAllPagesWithProgress(ctx, func(
		ctx context.Context, cursor *string,
	) ([]T, platformgithub.GraphQLPageInfo, error) {
		var q gqlPRQuery[T]
		vars := map[string]any{
			"owner":    githubv4.String(owner),
			"name":     githubv4.String(name),
			"pageSize": githubv4.Int(pageSize),
			"cursor":   cursorVar(cursor),
		}
		if err := client.Query(ctx, &q, vars); err != nil {
			return nil, platformgithub.GraphQLPageInfo{}, err
		}
		progress.setTotal(q.Repository.PullRequests.TotalCount)
		return q.Repository.PullRequests.Nodes,
			q.Repository.PullRequests.PageInfo, nil
	}, progress.recordPage)
}

func (g *GraphQLFetcher) FetchRepoIssues(
	ctx context.Context, owner, name string,
) (*RepoBulkResult, error) {
	result, err := g.fetchRepoIssuesWithPageSize(
		ctx, owner, name, topLevelPageSize,
	)
	if err != nil {
		slog.Warn("GraphQL issue query failed, retrying with smaller page",
			"owner", owner, "name", name,
			"err", err, "retryPageSize", retryPageSize,
		)
		result, err = g.fetchRepoIssuesWithPageSize(
			ctx, owner, name, retryPageSize,
		)
	}
	return result, err
}

func (g *GraphQLFetcher) fetchRepoIssuesWithPageSize(
	ctx context.Context, owner, name string, pageSize int,
) (*RepoBulkResult, error) {
	ctx = tokenauth.WithGitHubOwner(ctx, owner)
	progress := newIssueListFetchProgressLogger(RepoRef{
		Owner:        owner,
		Name:         name,
		PlatformHost: g.host,
	}, "graphql")
	gqlIssues, err := fetchAllPagesWithProgress(ctx, func(
		ctx context.Context, cursor *string,
	) ([]platformgithub.GraphQLIssue, platformgithub.GraphQLPageInfo, error) {
		var q gqlIssueQuery
		vars := map[string]any{
			"owner":    githubv4.String(owner),
			"name":     githubv4.String(name),
			"pageSize": githubv4.Int(pageSize),
			"cursor":   cursorVar(cursor),
		}
		if err := g.client.Query(ctx, &q, vars); err != nil {
			return nil, platformgithub.GraphQLPageInfo{}, err
		}
		progress.setTotal(q.Repository.Issues.TotalCount)
		return q.Repository.Issues.Nodes,
			q.Repository.Issues.PageInfo, nil
	}, progress.recordPage)
	if err != nil {
		return nil, err
	}
	progress.done()

	result := &RepoBulkResult{
		Issues: make([]BulkIssue, 0, len(gqlIssues)),
	}
	for i := range gqlIssues {
		bulk := convertGQLIssue(&gqlIssues[i])
		if err := g.completeIssueCommentVisibility(ctx, owner, name, &gqlIssues[i], &bulk); err != nil {
			return nil, err
		}
		result.Issues = append(result.Issues, bulk)
	}
	return result, nil
}

func (g *GraphQLFetcher) completePRComments(
	ctx context.Context,
	owner, name string,
	pr *platformgithub.GraphQLPR,
	bulk *BulkPR,
) error {
	if bulk.CommentsComplete {
		return nil
	}
	startAfter := nonEmptyCursor(pr.Comments.PageInfo.EndCursor)
	comments, err := fetchAllPages(ctx, func(
		ctx context.Context, cursor *string,
	) ([]platformgithub.GraphQLComment, platformgithub.GraphQLPageInfo, error) {
		var q gqlPRCommentPageQuery
		err := g.client.Query(ctx, &q, map[string]any{
			"owner": githubv4.String(owner), "name": githubv4.String(name),
			"number": githubv4.Int(pr.Number), "cursor": commentVisibilityCursor(startAfter, cursor),
		})
		if err != nil {
			return nil, platformgithub.GraphQLPageInfo{}, err
		}
		if q.Repository.PullRequest == nil {
			return nil, platformgithub.GraphQLPageInfo{}, fmt.Errorf("fetch comments for pull request #%d: missing pull request", pr.Number)
		}
		return q.Repository.PullRequest.Comments.Nodes,
			q.Repository.PullRequest.Comments.PageInfo, nil
	})
	if err != nil {
		return fmt.Errorf("paginate comments for pull request #%d: %w", pr.Number, err)
	}
	for i := range comments {
		comment := &comments[i]
		bulk.Comments = append(bulk.Comments, adaptComment(comment))
		bulk.CommentVisibility[platformgithub.FirstPositiveInt64(
			int64(comment.FullDatabaseId), comment.DatabaseId,
		)] = gqlCommentVisibility(comment)
	}
	bulk.CommentsComplete = true
	return nil
}

// FetchPullRequestCommentVisibility returns the current moderation state for
// every comment on a pull request, including closed pull requests and pages
// beyond the first 100 comments.
func (g *GraphQLFetcher) FetchPullRequestCommentVisibility(
	ctx context.Context,
	owner, name string,
	number int,
) (map[int64]platformgithub.CommentVisibility, error) {
	ctx = tokenauth.WithGitHubOwner(ctx, owner)
	return g.fetchPRCommentVisibility(ctx, owner, name, number, nil)
}

func (g *GraphQLFetcher) fetchPRCommentVisibility(
	ctx context.Context,
	owner, name string,
	number int,
	startAfter *string,
) (map[int64]platformgithub.CommentVisibility, error) {
	comments, err := fetchAllPages(ctx, func(
		ctx context.Context, cursor *string,
	) ([]platformgithub.GraphQLCommentVisibilityNode, platformgithub.GraphQLPageInfo, error) {
		var q gqlPRCommentVisibilityPageQuery
		err := g.client.Query(ctx, &q, map[string]any{
			"owner": githubv4.String(owner), "name": githubv4.String(name),
			"number": githubv4.Int(number), "cursor": commentVisibilityCursor(startAfter, cursor),
		})
		if err != nil {
			return nil, platformgithub.GraphQLPageInfo{}, err
		}
		if q.Repository.PullRequest == nil {
			return nil, platformgithub.GraphQLPageInfo{}, fmt.Errorf("fetch comments for pull request #%d: missing pull request", number)
		}
		return q.Repository.PullRequest.Comments.Nodes,
			q.Repository.PullRequest.Comments.PageInfo, nil
	})
	if err != nil {
		return nil, err
	}
	visibility := make(map[int64]platformgithub.CommentVisibility, len(comments))
	mergeCommentVisibility(visibility, comments)
	return visibility, nil
}

func (g *GraphQLFetcher) completeIssueCommentVisibility(
	ctx context.Context,
	owner, name string,
	issue *platformgithub.GraphQLIssue,
	bulk *BulkIssue,
) error {
	if bulk.CommentsComplete {
		return nil
	}
	startAfter := nonEmptyCursor(issue.Comments.PageInfo.EndCursor)
	visibility, err := g.fetchIssueCommentVisibility(ctx, owner, name, issue.Number, startAfter)
	if err != nil {
		return fmt.Errorf("paginate comments for issue #%d: %w", issue.Number, err)
	}
	mergeCommentVisibilityMap(bulk.CommentVisibility, visibility)
	return nil
}

// FetchIssueCommentVisibility returns the current moderation state for every
// comment on an issue, including closed issues and pages beyond the first 100
// comments.
func (g *GraphQLFetcher) FetchIssueCommentVisibility(
	ctx context.Context,
	owner, name string,
	number int,
) (map[int64]platformgithub.CommentVisibility, error) {
	ctx = tokenauth.WithGitHubOwner(ctx, owner)
	return g.fetchIssueCommentVisibility(ctx, owner, name, number, nil)
}

func (g *GraphQLFetcher) fetchIssueCommentVisibility(
	ctx context.Context,
	owner, name string,
	number int,
	startAfter *string,
) (map[int64]platformgithub.CommentVisibility, error) {
	comments, err := fetchAllPages(ctx, func(
		ctx context.Context, cursor *string,
	) ([]platformgithub.GraphQLCommentVisibilityNode, platformgithub.GraphQLPageInfo, error) {
		var q gqlIssueCommentPageQuery
		err := g.client.Query(ctx, &q, map[string]any{
			"owner": githubv4.String(owner), "name": githubv4.String(name),
			"number": githubv4.Int(number), "cursor": commentVisibilityCursor(startAfter, cursor),
		})
		if err != nil {
			return nil, platformgithub.GraphQLPageInfo{}, err
		}
		if q.Repository.Issue == nil {
			return nil, platformgithub.GraphQLPageInfo{}, fmt.Errorf("fetch comments for issue #%d: missing issue", number)
		}
		return q.Repository.Issue.Comments.Nodes,
			q.Repository.Issue.Comments.PageInfo, nil
	})
	if err != nil {
		return nil, err
	}
	visibility := make(map[int64]platformgithub.CommentVisibility, len(comments))
	mergeCommentVisibility(visibility, comments)
	return visibility, nil
}

func nonEmptyCursor(cursor string) *string {
	if cursor == "" {
		return nil
	}
	return &cursor
}

func commentVisibilityCursor(startAfter, cursor *string) *githubv4.String {
	if cursor != nil {
		return cursorVar(cursor)
	}
	return cursorVar(startAfter)
}

func mergeCommentVisibilityMap(
	dst, src map[int64]platformgithub.CommentVisibility,
) {
	maps.Copy(dst, src)
}

func mergeCommentVisibility(
	visibility map[int64]platformgithub.CommentVisibility,
	comments []platformgithub.GraphQLCommentVisibilityNode,
) {
	for i := range comments {
		comment := &comments[i]
		visibility[platformgithub.FirstPositiveInt64(
			int64(comment.FullDatabaseId), comment.DatabaseId,
		)] = commentVisibility(comment.IsMinimized, comment.MinimizedReason)
	}
}

func cursorVar(cursor *string) *githubv4.String {
	if cursor == nil {
		return nil
	}
	s := githubv4.String(*cursor)
	return &s
}

func convertGQLPR(gql *platformgithub.GraphQLPR) BulkPR {
	bulk := BulkPR{
		PR:                platformgithub.AdaptPR(gql),
		ReviewDecision:    gql.ReviewDecision,
		CommentVisibility: make(map[int64]platformgithub.CommentVisibility),
		CommentsComplete:  !gql.Comments.PageInfo.HasNextPage,
		ReviewsComplete:   !gql.Reviews.PageInfo.HasNextPage,
		ReviewThreadsComplete: platformgithub.GraphQLReviewThreadsComplete(
			gql.ReviewThreads.Nodes, gql.ReviewThreads.PageInfo.HasNextPage,
		),
		CommitsComplete:  !gql.AllCommits.PageInfo.HasNextPage,
		TimelineComplete: !gql.TimelineItems.PageInfo.HasNextPage,
	}

	for i := range gql.Comments.Nodes {
		comment := &gql.Comments.Nodes[i]
		bulk.Comments = append(bulk.Comments, adaptComment(comment))
		bulk.CommentVisibility[platformgithub.FirstPositiveInt64(
			int64(comment.FullDatabaseId), comment.DatabaseId,
		)] = gqlCommentVisibility(comment)
	}
	for i := range gql.Reviews.Nodes {
		bulk.Reviews = append(bulk.Reviews, adaptReview(&gql.Reviews.Nodes[i]))
	}
	bulk.ReviewThreads = platformReviewThreadsFromGQL(gql.ReviewThreads.Nodes)
	for i := range gql.AllCommits.Nodes {
		bulk.Commits = append(bulk.Commits, adaptCommit(&gql.AllCommits.Nodes[i]))
	}
	for i := range gql.TimelineItems.Nodes {
		event, ok := adaptPullRequestTimelineEvent(&gql.TimelineItems.Nodes[i])
		if ok {
			bulk.TimelineEvents = append(bulk.TimelineEvents, event)
		}
	}

	bulk.CIComplete = true
	if len(gql.LastCommit.Nodes) > 0 {
		rollup := gql.LastCommit.Nodes[0].Commit.StatusCheckRollup
		if rollup != nil {
			bulk.CIComplete = !rollup.Contexts.PageInfo.HasNextPage
			bulk.CheckRuns, bulk.Statuses = splitCheckContexts(
				rollup.Contexts.Nodes,
			)
		}
	}

	return bulk
}

func convertGQLPRWithNativeStacks(gql *platformgithub.GraphQLPRWithNativeStacks) BulkPR {
	bulk := convertGQLPR(&gql.GraphQLPR)
	if gql.Stack != nil && gql.StackEntry != nil {
		bulk.NativeStack = &platformgithub.NativeStackHint{
			Number: gql.Stack.Number, Size: gql.Stack.Size,
			Position: gql.StackEntry.Position, BaseRef: gql.Stack.BaseRefName,
		}
	}
	return bulk
}

func adaptIssueTimelineEvent(gql *platformgithub.GraphQLIssueTimelineItem) (platformgithub.PullRequestTimelineEvent, bool) {
	if gql == nil {
		return platformgithub.PullRequestTimelineEvent{}, false
	}
	event := platformgithub.PullRequestTimelineEvent{NodeID: gql.Node.ID}
	switch gql.Typename {
	case "CrossReferencedEvent":
		copyCrossReferencedEvent(&event, gql.CrossReferencedEvent)
	case "AssignedEvent":
		copyAssignmentEvent(&event, "assigned", gql.AssignedEvent)
	case "UnassignedEvent":
		copyAssignmentEvent(&event, "unassigned", gql.UnassignedEvent)
	case "ClosedEvent":
		copyLifecycleEvent(&event, "closed", gql.ClosedEvent)
	case "ReopenedEvent":
		copyLifecycleEvent(&event, "reopened", gql.ReopenedEvent)
	default:
		return platformgithub.PullRequestTimelineEvent{}, false
	}
	return event, true
}

func adaptPullRequestTimelineEvent(gql *platformgithub.GraphQLPullRequestTimelineItem) (platformgithub.PullRequestTimelineEvent, bool) {
	if gql == nil {
		return platformgithub.PullRequestTimelineEvent{}, false
	}
	event := platformgithub.PullRequestTimelineEvent{NodeID: gql.Node.ID}
	switch gql.Typename {
	case "HeadRefForcePushedEvent":
		src := gql.HeadRefForcePushedEvent
		event.EventType = "force_push"
		event.CreatedAt = src.CreatedAt
		if src.Actor != nil {
			event.Actor = src.Actor.Login
		}
		if src.BeforeCommit != nil {
			event.BeforeSHA = src.BeforeCommit.OID
		}
		if src.AfterCommit != nil {
			event.AfterSHA = src.AfterCommit.OID
		}
		if src.Ref != nil {
			event.Ref = src.Ref.Name
		}
	case "CommentDeletedEvent":
		src := gql.CommentDeletedEvent
		event.EventType = "comment_deleted"
		event.CreatedAt = src.CreatedAt
		if src.Actor != nil {
			event.Actor = src.Actor.Login
		}
		if src.DeletedCommentAuthor != nil {
			event.DeletedCommentAuthor = src.DeletedCommentAuthor.Login
		}
	case "CrossReferencedEvent":
		copyCrossReferencedEvent(&event, gql.CrossReferencedEvent)
	case "RenamedTitleEvent":
		src := gql.RenamedTitleEvent
		event.EventType = "renamed_title"
		event.CreatedAt = src.CreatedAt
		event.PreviousTitle = src.PreviousTitle
		event.CurrentTitle = src.CurrentTitle
		if src.Actor != nil {
			event.Actor = src.Actor.Login
		}
	case "BaseRefChangedEvent":
		src := gql.BaseRefChangedEvent
		event.EventType = "base_ref_changed"
		event.CreatedAt = src.CreatedAt
		event.PreviousRefName = src.PreviousRefName
		event.CurrentRefName = src.CurrentRefName
		if src.Actor != nil {
			event.Actor = src.Actor.Login
		}
	case "AssignedEvent":
		copyAssignmentEvent(&event, "assigned", gql.AssignedEvent)
	case "UnassignedEvent":
		copyAssignmentEvent(&event, "unassigned", gql.UnassignedEvent)
	case "MergedEvent":
		copyLifecycleEvent(&event, "merged", gql.MergedEvent)
	case "ClosedEvent":
		copyLifecycleEvent(&event, "closed", gql.ClosedEvent)
	case "ReopenedEvent":
		copyLifecycleEvent(&event, "reopened", gql.ReopenedEvent)
	default:
		return platformgithub.PullRequestTimelineEvent{}, false
	}
	return event, true
}

func copyAssignmentEvent(event *platformgithub.PullRequestTimelineEvent, eventType string, src platformgithub.GraphQLAssignedEvent) {
	event.EventType = eventType
	event.Assignee = src.Assignee.Login()
	event.CreatedAt = src.CreatedAt
	if src.Actor != nil {
		event.Actor = src.Actor.Login
	}
}

func copyLifecycleEvent(event *platformgithub.PullRequestTimelineEvent, eventType string, src platformgithub.GraphQLLifecycleEvent) {
	event.EventType = eventType
	event.CreatedAt = src.CreatedAt
	if src.Actor != nil {
		event.Actor = src.Actor.Login
	}
}

func copyCrossReferencedEvent(event *platformgithub.PullRequestTimelineEvent, src platformgithub.GraphQLCrossReferencedEvent) {
	event.EventType = "cross_referenced"
	event.CreatedAt = src.CreatedAt
	event.IsCrossRepository = src.IsCrossRepository
	event.WillCloseTarget = src.WillCloseTarget
	if src.Actor != nil {
		event.Actor = src.Actor.Login
	}
	event.SourceType = src.Source.Typename
	switch src.Source.Typename {
	case "Issue":
		copyReferencedSubject(event, src.Source.Issue)
	case "PullRequest":
		copyReferencedSubject(event, src.Source.PullRequest)
	}
}

func copyReferencedSubject(event *platformgithub.PullRequestTimelineEvent, source platformgithub.GraphQLReferencedIssueOrPR) {
	event.SourceNumber = source.Number
	event.SourceTitle = source.Title
	event.SourceURL = source.URL
	event.SourceOwner = source.Repository.Owner.Login
	event.SourceRepo = source.Repository.Name
}
