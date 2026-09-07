package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/platform"
)

const githubPageSize = 100

type githubArchiveReviewCursor struct {
	Host         string                          `json:"host"`
	Owner        string                          `json:"owner"`
	Repo         string                          `json:"repo"`
	Number       int                             `json:"number"`
	Phase        string                          `json:"phase"`
	ThreadAfter  string                          `json:"thread_after,omitempty"`
	CommentAfter string                          `json:"comment_after,omitempty"`
	MoreThreads  bool                            `json:"more_threads,omitempty"`
	Thread       githubArchiveReviewThreadCursor `json:"thread,omitzero"`
}

type githubArchiveReviewThreadCursor struct {
	NodeID            string `json:"node_id"`
	IsResolved        bool   `json:"is_resolved,omitempty"`
	IsOutdated        bool   `json:"is_outdated,omitempty"`
	Path              string `json:"path,omitempty"`
	Side              string `json:"side,omitempty"`
	StartLine         *int   `json:"start_line,omitempty"`
	OriginalStartLine *int   `json:"original_start_line,omitempty"`
	Line              int    `json:"line,omitempty"`
	OriginalLine      int    `json:"original_line,omitempty"`
}

const archiveReviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        edges {
          cursor
          node {
            id isResolved isOutdated path line originalLine startLine originalStartLine diffSide
            comments(first: 100) {
              nodes { id databaseId fullDatabaseId pullRequestReview { databaseId } subjectType body author { login } path line originalLine diffHunk url commit { oid } originalCommit { oid } isMinimized minimizedReason createdAt updatedAt }
              pageInfo { hasNextPage endCursor }
            }
          }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const archiveIssuesQuery = `
query($owner: String!, $repo: String!, $cursor: String, $orderField: IssueOrderField!, $since: DateTime) {
  repository(owner: $owner, name: $repo) {
    issues(first: 100, after: $cursor, states: [OPEN, CLOSED], filterBy: {since: $since}, orderBy: {field: $orderField, direction: ASC}) {
      nodes {
        id databaseId number title state body url createdAt updatedAt closedAt
        author { login }
        comments { totalCount }
        labels(first: 100) { nodes { name color description isDefault } }
        assignees(first: 100) { nodes { login } }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

type githubArchiveIssueNode struct {
	NodeID     string     `json:"id"`
	DatabaseID int64      `json:"databaseId"`
	Number     int        `json:"number"`
	Title      string     `json:"title"`
	State      string     `json:"state"`
	Body       string     `json:"body"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	ClosedAt   *time.Time `json:"closedAt"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	Comments struct {
		TotalCount int `json:"totalCount"`
	} `json:"comments"`
	Labels struct {
		Nodes []struct {
			Name        string `json:"name"`
			Color       string `json:"color"`
			Description string `json:"description"`
			IsDefault   bool   `json:"isDefault"`
		} `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
}

func (c *Client) ListInventoryIssuesPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	cursor string,
	since string,
) ([]*gh.Issue, string, bool, error) {
	orderField := "CREATED_AT"
	if sortBy == "updated" {
		orderField = "UPDATED_AT"
	}
	type response struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				Issues struct {
					Nodes    []githubArchiveIssueNode `json:"nodes"`
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	var decoded response
	ctx = c.authContext(ctx, owner, false)
	if err := c.doArchiveGraphQL(ctx, archiveIssuesQuery, map[string]any{
		"owner": owner, "repo": repo, "cursor": nullableCursor(cursor), "orderField": orderField,
		"since": nullableCursor(since),
	}, &decoded, &decoded.Errors); err != nil {
		return nil, "", false, fmt.Errorf("list archive issues for %s/%s: %w", owner, repo, err)
	}
	if decoded.Data.Repository == nil {
		return nil, "", false, errors.New("missing repository in archive issue response")
	}
	connection := decoded.Data.Repository.Issues
	items := make([]*gh.Issue, 0, len(connection.Nodes))
	for i := range connection.Nodes {
		items = append(items, githubArchiveIssueFromGraphQL(&connection.Nodes[i]))
	}
	return items, cursorValue(connection.PageInfo.EndCursor), !connection.PageInfo.HasNextPage, nil
}

func (c *Client) ListInventoryPullRequestsPage(
	ctx context.Context,
	owner string,
	repo string,
	sortBy string,
	page int,
) ([]*gh.PullRequest, bool, error) {
	direction := "asc"
	if sortBy == "updated" {
		direction = "desc"
	}
	opts := &gh.PullRequestListOptions{
		State:     "all",
		Sort:      sortBy,
		Direction: direction,
		Page:      page, PerPage: githubPageSize,
	}
	items, resp, err := c.gh.PullRequests.List(WithUnconditionalRead(ctx), owner, repo, opts)
	c.trackRate(resp)
	if err != nil {
		return nil, false, fmt.Errorf("list archive pull requests for %s/%s: %w", owner, repo, err)
	}
	return items, resp != nil && resp.NextPage > 0, nil
}

func githubArchiveIssueFromGraphQL(node *githubArchiveIssueNode) *gh.Issue {
	state := strings.ToLower(node.State)
	issue := &gh.Issue{
		ID: new(node.DatabaseID), NodeID: new(node.NodeID), Number: new(node.Number),
		Title: new(node.Title), State: new(state), Body: new(node.Body), HTMLURL: new(node.URL),
		Comments:  new(node.Comments.TotalCount),
		CreatedAt: &gh.Timestamp{Time: node.CreatedAt}, UpdatedAt: &gh.Timestamp{Time: node.UpdatedAt},
	}
	if node.Author != nil {
		issue.User = &gh.User{Login: new(node.Author.Login)}
	}
	if node.ClosedAt != nil {
		issue.ClosedAt = &gh.Timestamp{Time: *node.ClosedAt}
	}
	for _, label := range node.Labels.Nodes {
		issue.Labels = append(issue.Labels, &gh.Label{
			Name: new(label.Name), Color: new(label.Color), Description: new(label.Description),
			Default: new(label.IsDefault),
		})
	}
	for _, assignee := range node.Assignees.Nodes {
		issue.Assignees = append(issue.Assignees, &gh.User{Login: new(assignee.Login)})
	}
	return issue
}

func (c *Client) ListIssueCommentsPage(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	page int,
) ([]*gh.IssueComment, bool, error) {
	items, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, &gh.IssueListCommentsOptions{
		Page: page, PerPage: githubPageSize,
	})
	c.trackRate(resp)
	if err != nil {
		return nil, false, fmt.Errorf("list archive comments for %s/%s#%d: %w", owner, repo, number, err)
	}
	return items, resp != nil && resp.NextPage > 0, nil
}

func (c *Client) ListReviewsPage(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	page int,
) ([]*gh.PullRequestReview, bool, error) {
	items, resp, err := c.gh.PullRequests.ListReviews(
		ctx, owner, repo, number,
		&gh.ListOptions{Page: page, PerPage: githubPageSize},
	)
	c.trackRate(resp)
	if err != nil {
		return nil, false, fmt.Errorf("list archive reviews for %s/%s#%d: %w", owner, repo, number, err)
	}
	return items, resp != nil && resp.NextPage > 0, nil
}

func (c *Client) ListInventoryReviewThreadsPage(
	ctx context.Context,
	host string,
	owner string,
	repo string,
	number int,
	cursor string,
) ([]PullRequestReviewThread, string, bool, error) {
	state, err := decodeGitHubArchiveReviewCursor(cursor, host, owner, repo, number)
	if err != nil {
		return nil, "", false, err
	}
	if state.Phase == "comments" {
		return c.listReviewThreadCommentsPage(ctx, owner, repo, number, state)
	}
	ctx = c.authContext(ctx, owner, false)
	type response struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Edges []struct {
							Cursor string `json:"cursor"`
							Node   struct {
								NodeID            string                               `json:"id"`
								IsResolved        bool                                 `json:"isResolved"`
								IsOutdated        bool                                 `json:"isOutdated"`
								Path              string                               `json:"path"`
								Line              int                                  `json:"line"`
								OriginalLine      int                                  `json:"originalLine"`
								StartLine         *int                                 `json:"startLine"`
								OriginalStartLine *int                                 `json:"originalStartLine"`
								Side              string                               `json:"diffSide"`
								Comments          graphQLReviewThreadCommentConnection `json:"comments"`
							} `json:"node"`
						} `json:"edges"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	var decoded response
	after := nullableCursor(state.ThreadAfter)
	if err := c.doArchiveGraphQL(ctx, archiveReviewThreadsQuery, map[string]any{
		"owner": owner, "repo": repo, "number": number, "cursor": after,
	}, &decoded, &decoded.Errors); err != nil {
		return nil, "", false, fmt.Errorf("list archive review threads for %s/%s#%d: %w", owner, repo, number, err)
	}
	if decoded.Data.Repository == nil || decoded.Data.Repository.PullRequest == nil {
		return nil, "", false, errors.New("missing pull request in archive review response")
	}
	connection := decoded.Data.Repository.PullRequest.ReviewThreads
	if len(connection.Edges) == 0 {
		return nil, "", true, nil
	}
	threads := make([]PullRequestReviewThread, 0, len(connection.Edges))
	for i, edge := range connection.Edges {
		node := edge.Node
		thread := PullRequestReviewThread{
			NodeID: node.NodeID, IsResolved: node.IsResolved, IsOutdated: node.IsOutdated,
			Path: node.Path, Side: node.Side, StartLine: node.StartLine,
			OriginalStartLine: node.OriginalStartLine, Line: node.Line, OriginalLine: node.OriginalLine,
		}
		for _, comment := range node.Comments.Nodes {
			thread.Comments = append(thread.Comments, githubReviewThreadCommentFromGraphQL(comment))
		}
		threads = append(threads, thread)
		if node.Comments.PageInfo.HasNextPage {
			next, err := encodeGitHubArchiveReviewCursor(githubArchiveReviewCursor{
				Host: host, Owner: owner, Repo: repo, Number: number,
				Phase: "comments", CommentAfter: cursorValue(node.Comments.PageInfo.EndCursor),
				ThreadAfter: edge.Cursor,
				MoreThreads: i < len(connection.Edges)-1 || connection.PageInfo.HasNextPage,
				Thread:      archiveReviewThreadCursor(thread),
			})
			return threads, next, false, err
		}
	}
	if !connection.PageInfo.HasNextPage {
		return threads, "", true, nil
	}
	next, err := encodeGitHubArchiveReviewCursor(githubArchiveReviewCursor{
		Host: host, Owner: owner, Repo: repo, Number: number,
		Phase: "threads", ThreadAfter: cursorValue(connection.PageInfo.EndCursor),
	})
	return threads, next, false, err
}

func (c *Client) listReviewThreadCommentsPage(
	ctx context.Context,
	owner string,
	repo string,
	number int,
	state githubArchiveReviewCursor,
) ([]PullRequestReviewThread, string, bool, error) {
	ctx = c.authContext(ctx, owner, false)
	type response struct {
		Errors []graphQLError `json:"errors"`
		Data   struct {
			Node *struct {
				Comments graphQLReviewThreadCommentConnection `json:"comments"`
			} `json:"node"`
		} `json:"data"`
	}
	var decoded response
	if err := c.doArchiveGraphQL(ctx, pullRequestReviewThreadCommentsQuery, map[string]any{
		"threadID": state.Thread.NodeID, "cursor": nullableCursor(state.CommentAfter),
	}, &decoded, &decoded.Errors); err != nil {
		return nil, "", false, fmt.Errorf("list archive review thread comments for %s/%s#%d: %w", owner, repo, number, err)
	}
	if decoded.Data.Node == nil {
		return nil, "", false, errors.New("missing review thread in archive comment response")
	}
	thread := state.Thread.thread()
	for _, comment := range decoded.Data.Node.Comments.Nodes {
		thread.Comments = append(thread.Comments, githubReviewThreadCommentFromGraphQL(comment))
	}
	pageInfo := decoded.Data.Node.Comments.PageInfo
	if pageInfo.HasNextPage {
		state.CommentAfter = cursorValue(pageInfo.EndCursor)
		next, err := encodeGitHubArchiveReviewCursor(state)
		return []PullRequestReviewThread{thread}, next, false, err
	}
	if state.MoreThreads {
		next, err := encodeGitHubArchiveReviewCursor(githubArchiveReviewCursor{
			Host: state.Host, Owner: owner, Repo: repo, Number: number,
			Phase: "threads", ThreadAfter: state.ThreadAfter,
		})
		return []PullRequestReviewThread{thread}, next, false, err
	}
	return []PullRequestReviewThread{thread}, "", true, nil
}

func (c *Client) doArchiveGraphQL(
	ctx context.Context,
	query string,
	variables map[string]any,
	out any,
	graphQLErrors *[]graphQLError,
) error {
	payload, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		c.graphQLContext(ctx),
		http.MethodPost,
		c.graphQLEndpoint,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.trackGraphQLRateHeaders(resp)
	if resp.StatusCode != http.StatusOK {
		return gh.CheckResponse(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return err
	}
	if len(*graphQLErrors) > 0 {
		return githubArchiveGraphQLErrors(c.platformHost, resp, *graphQLErrors)
	}
	return nil
}

func githubArchiveGraphQLErrors(host string, resp *http.Response, graphQLErrors []graphQLError) error {
	cause := fmt.Errorf("graphql errors: %s", joinGraphQLErrorMessages(graphQLErrors))
	for _, graphQLError := range graphQLErrors {
		switch strings.ToUpper(graphQLError.Type) {
		case "RATE_LIMITED":
			return &platform.Error{
				Code: platform.ErrCodeRateLimited, Provider: platform.KindGitHub,
				PlatformHost: host, ResetAt: ArchiveResetAt(resp), Err: cause,
			}
		case "FORBIDDEN", "UNAUTHORIZED":
			return platform.PermissionDenied(platform.KindGitHub, host, cause)
		}
	}
	return cause
}

func ArchiveResetAt(resp *http.Response) *time.Time {
	if resp == nil {
		return nil
	}
	value, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || value <= 0 {
		return nil
	}
	reset := time.Unix(value, 0).UTC()
	return &reset
}

func decodeGitHubArchiveReviewCursor(
	encoded string,
	host string,
	owner string,
	repo string,
	number int,
) (githubArchiveReviewCursor, error) {
	if encoded == "" {
		return githubArchiveReviewCursor{
			Host: host, Owner: owner, Repo: repo, Number: number, Phase: "threads",
		}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return githubArchiveReviewCursor{}, fmt.Errorf("decode review cursor: %w", err)
	}
	var cursor githubArchiveReviewCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return githubArchiveReviewCursor{}, fmt.Errorf("parse review cursor: %w", err)
	}
	if cursor.Host != host || cursor.Owner != owner || cursor.Repo != repo || cursor.Number != number {
		return githubArchiveReviewCursor{}, errors.New("review cursor does not match archive item")
	}
	if cursor.Phase != "threads" && cursor.Phase != "comments" {
		return githubArchiveReviewCursor{}, errors.New("invalid review cursor phase")
	}
	if cursor.Phase == "comments" && (cursor.Thread.NodeID == "" || cursor.CommentAfter == "") {
		return githubArchiveReviewCursor{}, errors.New("incomplete review comment cursor")
	}
	return cursor, nil
}

func encodeGitHubArchiveReviewCursor(cursor githubArchiveReviewCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode review cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func archiveReviewThreadCursor(thread PullRequestReviewThread) githubArchiveReviewThreadCursor {
	return githubArchiveReviewThreadCursor{
		NodeID: thread.NodeID, IsResolved: thread.IsResolved, IsOutdated: thread.IsOutdated,
		Path: thread.Path, Side: thread.Side, StartLine: thread.StartLine,
		OriginalStartLine: thread.OriginalStartLine, Line: thread.Line, OriginalLine: thread.OriginalLine,
	}
}

func (cursor githubArchiveReviewThreadCursor) thread() PullRequestReviewThread {
	return PullRequestReviewThread{
		NodeID: cursor.NodeID, IsResolved: cursor.IsResolved, IsOutdated: cursor.IsOutdated,
		Path: cursor.Path, Side: cursor.Side, StartLine: cursor.StartLine,
		OriginalStartLine: cursor.OriginalStartLine, Line: cursor.Line, OriginalLine: cursor.OriginalLine,
	}
}

func nullableCursor(cursor string) any {
	if cursor == "" {
		return nil
	}
	return cursor
}
