package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v89/github"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
)

var errFixtureReadOnly = errors.New("fixture client: mutation not supported")
var errFixtureNotFound = errors.New("fixture client: not found")

type fixtureReadyForReviewStaleStateError struct {
	message string
}

func (e *fixtureReadyForReviewStaleStateError) Error() string      { return e.message }
func (e *fixtureReadyForReviewStaleStateError) StatusCode() int    { return http.StatusNotFound }
func (e *fixtureReadyForReviewStaleStateError) IsStaleState() bool { return true }

// FixtureClient is a ghclient.Client implementation for E2E tests. It serves
// seeded PRs and issues from the list methods and stubs out everything else.
type FixtureClient struct {
	OpenPRs                   map[string][]*gh.PullRequest
	PRs                       map[string][]*gh.PullRequest
	OpenIssues                map[string][]*gh.Issue
	Issues                    map[string][]*gh.Issue
	Comments                  map[string][]*gh.IssueComment
	Reviews                   map[string][]*gh.PullRequestReview
	ReviewThreads             map[string][]platformgithub.PullRequestReviewThread
	ReposByOwner              map[string][]*gh.Repository
	Releases                  map[string][]*gh.RepositoryRelease
	Tags                      map[string][]*gh.RepositoryTag
	CombinedStatuses          map[string]*gh.CombinedStatus
	CheckRuns                 map[string][]*gh.CheckRun
	Labels                    map[string][]*gh.Label
	CheckRunErrors            map[string]error
	WorkflowRuns              map[string][]*gh.WorkflowRun
	Notifications             []ghclient.NotificationThread
	MarkReadNotifications     []string
	ListRepositoriesByOwnerFn func(context.Context, string) ([]*gh.Repository, error)
	mu                        sync.RWMutex
	nextID                    int64
	mergePullRequestError     error
	mergePullRequestResult    *gh.PullRequestMergeResult
	reviewSuggestionResult    *platform.AppliedReviewSuggestions
	reviewSuggestionBaseSHA   string
	issueCommentsGateMu       sync.Mutex
	issueCommentsGate         chan struct{}
}

// NewFixtureClient returns a FixtureClient with empty fixture maps.
func NewFixtureClient() ghclient.Client {
	return &FixtureClient{
		OpenPRs:          make(map[string][]*gh.PullRequest),
		PRs:              make(map[string][]*gh.PullRequest),
		OpenIssues:       make(map[string][]*gh.Issue),
		Issues:           make(map[string][]*gh.Issue),
		Comments:         make(map[string][]*gh.IssueComment),
		Reviews:          make(map[string][]*gh.PullRequestReview),
		ReviewThreads:    make(map[string][]platformgithub.PullRequestReviewThread),
		ReposByOwner:     make(map[string][]*gh.Repository),
		Releases:         make(map[string][]*gh.RepositoryRelease),
		Tags:             make(map[string][]*gh.RepositoryTag),
		CombinedStatuses: make(map[string]*gh.CombinedStatus),
		CheckRuns:        make(map[string][]*gh.CheckRun),
		Labels:           make(map[string][]*gh.Label),
		CheckRunErrors:   make(map[string]error),
		WorkflowRuns:     make(map[string][]*gh.WorkflowRun),
		nextID:           10_000,
	}
}

func (c *FixtureClient) ListNotifications(
	_ context.Context, _ ghclient.NotificationListOptions,
) ([]ghclient.NotificationThread, bool, error) {
	out := make([]ghclient.NotificationThread, len(c.Notifications))
	copy(out, c.Notifications)
	return out, false, nil
}

func (c *FixtureClient) MarkNotificationThreadRead(
	_ context.Context, threadID string,
) error {
	c.MarkReadNotifications = append(c.MarkReadNotifications, threadID)
	return nil
}

func (c *FixtureClient) AuthenticatedViewerLogin(context.Context) (string, error) {
	return "fixture-viewer", nil
}

func (c *FixtureClient) SetMergePullRequestError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergePullRequestError = err
}

func (c *FixtureClient) SetMergePullRequestResult(result *gh.PullRequestMergeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergePullRequestResult = result
}

func (c *FixtureClient) SetReviewSuggestionResult(
	result *platform.AppliedReviewSuggestions,
	baseSHA string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reviewSuggestionResult = result
	c.reviewSuggestionBaseSHA = baseSHA
}

func repoKey(owner, repo string) string {
	return fmt.Sprintf("%s/%s", owner, repo)
}

func issueKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

func refKey(owner, repo, ref string) string {
	return fmt.Sprintf("%s/%s@%s", owner, repo, ref)
}

// ListOpenPullRequests returns the seeded open PRs for the given repo.
func (c *FixtureClient) ListOpenPullRequests(
	_ context.Context, owner, repo string,
) ([]*gh.PullRequest, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return clonePullRequests(c.OpenPRs[repoKey(owner, repo)]), nil
}

// ListOpenIssues returns the seeded open issues for the given repo.
func (c *FixtureClient) ListOpenIssues(
	_ context.Context, owner, repo string,
) ([]*gh.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return slices.Clone(c.OpenIssues[repoKey(owner, repo)]), nil
}

// GetUser returns a stub user with the given login.
func (c *FixtureClient) GetUser(_ context.Context, login string) (*gh.User, error) {
	return &gh.User{Login: &login}, nil
}

func (c *FixtureClient) ListRepositoriesByOwner(
	ctx context.Context, owner string,
) ([]*gh.Repository, error) {
	if c.ListRepositoriesByOwnerFn != nil {
		return c.ListRepositoriesByOwnerFn(ctx, owner)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	repos := c.ReposByOwner[owner]
	if len(repos) == 0 {
		return nil, nil
	}
	return slices.Clone(repos), nil
}

func (c *FixtureClient) ListReleases(
	_ context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryRelease, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	releases := c.Releases[repoKey(owner, repo)]
	if len(releases) == 0 {
		return nil, nil
	}
	if perPage > 0 && perPage < len(releases) {
		releases = releases[:perPage]
	}
	return slices.Clone(releases), nil
}

func (c *FixtureClient) ListTags(
	_ context.Context, owner, repo string, perPage int,
) ([]*gh.RepositoryTag, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tags := c.Tags[repoKey(owner, repo)]
	if len(tags) == 0 {
		return nil, nil
	}
	if perPage > 0 && perPage < len(tags) {
		tags = tags[:perPage]
	}
	return slices.Clone(tags), nil
}

// GetRepository returns a repository with all merge methods enabled.
func (c *FixtureClient) GetRepository(
	_ context.Context, owner, repo string,
) (*gh.Repository, error) {
	t := true
	archived := repo == "archived"
	nodeID := "repo-" + owner + "-" + repo
	return &gh.Repository{
		Name:             &repo,
		NodeID:           &nodeID,
		Owner:            &gh.User{Login: &owner},
		Archived:         &archived,
		AllowSquashMerge: &t,
		AllowMergeCommit: &t,
		AllowRebaseMerge: &t,
	}, nil
}

// GetPullRequest looks up the PR by owner/repo and number from
// the seeded fixture set. Returns nil, nil if not found.
func (c *FixtureClient) GetPullRequest(
	_ context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, pr := range c.PRs[repoKey(owner, repo)] {
		if pr.GetNumber() == number {
			return clonePullRequest(pr), nil
		}
	}
	return nil, nil
}

func (c *FixtureClient) findPullRequest(owner, repo string, number int) *gh.PullRequest {
	for _, pr := range c.OpenPRs[repoKey(owner, repo)] {
		if pr.GetNumber() == number {
			return pr
		}
	}
	return nil
}

func (c *FixtureClient) updatePullRequestDraft(owner, repo string, number int, draft bool) *gh.PullRequest {
	var updated *gh.PullRequest
	now := gh.Timestamp{Time: time.Now().UTC()}
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() != number {
				continue
			}
			pr.Draft = new(draft)
			pr.UpdatedAt = &now
			if updated == nil {
				updated = pr
			}
		}
	}
	return updated
}

// GetIssue looks up the issue by owner/repo and number from
// the seeded fixture set. Returns nil, nil if not found.
func (c *FixtureClient) GetIssue(
	_ context.Context, owner, repo string, number int,
) (*gh.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, iss := range c.Issues[repoKey(owner, repo)] {
		if iss.GetNumber() == number {
			return iss, nil
		}
	}
	return nil, nil
}

func (c *FixtureClient) ListRepoLabels(
	_ context.Context, owner, repo string,
) ([]*gh.Label, error) {
	return slices.Clone(c.Labels[repoKey(owner, repo)]), nil
}

func (c *FixtureClient) ReplaceIssueLabels(
	_ context.Context, owner, repo string, number int, names []string,
) ([]*gh.Label, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	catalog := c.Labels[repoKey(owner, repo)]
	byName := make(map[string]*gh.Label, len(catalog))
	for _, label := range catalog {
		if label != nil {
			byName[label.GetName()] = label
		}
	}
	labels := make([]*gh.Label, 0, len(names))
	for _, name := range names {
		label, ok := byName[name]
		if !ok {
			return nil, errFixtureNotFound
		}
		labels = append(labels, label)
	}
	for _, prs := range [][]*gh.PullRequest{c.OpenPRs[repoKey(owner, repo)], c.PRs[repoKey(owner, repo)]} {
		for _, pr := range prs {
			if pr.GetNumber() == number {
				pr.Labels = slices.Clone(labels)
			}
		}
	}
	for _, issues := range [][]*gh.Issue{c.OpenIssues[repoKey(owner, repo)], c.Issues[repoKey(owner, repo)]} {
		for _, issue := range issues {
			if issue.GetNumber() == number {
				issue.Labels = slices.Clone(labels)
			}
		}
	}
	return slices.Clone(labels), nil
}

// fixtureRejectedUsername is refused by assignee and reviewer
// mutations, mirroring how providers reject usernames that do not
// exist. E2E tests use it to exercise failed-save error handling.
const fixtureRejectedUsername = "ghost"

// ReplaceIssueAssignees replaces the assignee set on the seeded issue or
// pull request with the given number, mirroring GitHub's issues API
// which covers both item kinds.
func (c *FixtureClient) ReplaceIssueAssignees(
	_ context.Context, owner, repo string, number int, usernames []string,
) (*gh.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if slices.Contains(usernames, fixtureRejectedUsername) {
		return nil, fmt.Errorf("%w: user %q", errFixtureNotFound, fixtureRejectedUsername)
	}

	users := make([]*gh.User, 0, len(usernames))
	for _, username := range usernames {
		login := username
		users = append(users, &gh.User{Login: &login})
	}
	now := gh.Timestamp{Time: time.Now().UTC()}
	var updatedIssue *gh.Issue
	for _, issues := range [][]*gh.Issue{c.OpenIssues[repoKey(owner, repo)], c.Issues[repoKey(owner, repo)]} {
		for _, issue := range issues {
			if issue.GetNumber() == number {
				issue.Assignees = slices.Clone(users)
				issue.UpdatedAt = &now
				if updatedIssue == nil {
					updatedIssue = issue
				}
			}
		}
	}
	var updatedPR *gh.PullRequest
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() == number {
				pr.Assignees = slices.Clone(users)
				pr.UpdatedAt = &now
				if updatedPR == nil {
					updatedPR = pr
				}
			}
		}
	}
	if updatedIssue != nil {
		return cloneFixtureValue(updatedIssue), nil
	}
	if updatedPR == nil {
		return nil, fmt.Errorf("%w: issue %s/%s#%d", errFixtureNotFound, owner, repo, number)
	}
	return &gh.Issue{
		Number:    updatedPR.Number,
		Assignees: slices.Clone(users),
	}, nil
}

// RequestPullRequestReviewers adds pending review requests on the seeded
// pull request and returns its updated state.
func (c *FixtureClient) RequestPullRequestReviewers(
	_ context.Context, owner, repo string, number int, usernames []string,
) (*gh.PullRequest, error) {
	if slices.Contains(usernames, fixtureRejectedUsername) {
		return nil, fmt.Errorf("%w: user %q", errFixtureNotFound, fixtureRejectedUsername)
	}
	return c.mutateRequestedReviewers(owner, repo, number, func(current []*gh.User) []*gh.User {
		next := slices.Clone(current)
		for _, username := range usernames {
			exists := slices.ContainsFunc(next, func(u *gh.User) bool {
				return u.GetLogin() == username
			})
			if !exists {
				login := username
				next = append(next, &gh.User{Login: &login})
			}
		}
		return next
	})
}

// RemovePullRequestReviewers removes pending review requests on the
// seeded pull request.
func (c *FixtureClient) RemovePullRequestReviewers(
	_ context.Context, owner, repo string, number int, usernames []string,
) error {
	_, err := c.mutateRequestedReviewers(owner, repo, number, func(current []*gh.User) []*gh.User {
		next := make([]*gh.User, 0, len(current))
		for _, user := range current {
			if !slices.Contains(usernames, user.GetLogin()) {
				next = append(next, user)
			}
		}
		return next
	})
	return err
}

func (c *FixtureClient) mutateRequestedReviewers(
	owner, repo string,
	number int,
	apply func(current []*gh.User) []*gh.User,
) (*gh.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := gh.Timestamp{Time: time.Now().UTC()}
	var updated *gh.PullRequest
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() != number {
				continue
			}
			pr.RequestedReviewers = apply(pr.RequestedReviewers)
			pr.UpdatedAt = &now
			if updated == nil {
				updated = pr
			}
		}
	}
	if updated == nil {
		return nil, fmt.Errorf("%w: pull request %s/%s#%d", errFixtureNotFound, owner, repo, number)
	}
	return clonePullRequest(updated), nil
}

func (c *FixtureClient) CreateIssue(
	_ context.Context, owner, repo, title, body string,
) (*gh.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	issuesKey := repoKey(owner, repo)
	maxNumber := 0
	for _, issue := range c.Issues[issuesKey] {
		if n := issue.GetNumber(); n > maxNumber {
			maxNumber = n
		}
	}

	number := maxNumber + 1
	now := gh.Timestamp{Time: time.Now().UTC()}
	state := "open"
	id := c.nextID
	c.nextID++
	labelID := c.nextID
	c.nextID++
	htmlURL := fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, number)
	login := "fixture-bot"
	comments := 0
	labelName := "created-from-repos"
	labelDescription := "Issue created from the repositories page"
	labelColor := "0e8a16"
	labelDefault := false

	issue := &gh.Issue{
		ID:               &id,
		Number:           &number,
		Title:            &title,
		Body:             &body,
		State:            &state,
		HTMLURL:          &htmlURL,
		User:             &gh.User{Login: &login},
		Comments:         &comments,
		CreatedAt:        &now,
		UpdatedAt:        &now,
		ClosedAt:         nil,
		PullRequestLinks: nil,
		Labels: []*gh.Label{{
			ID:          &labelID,
			Name:        &labelName,
			Description: &labelDescription,
			Color:       &labelColor,
			Default:     &labelDefault,
		}},
	}
	c.Issues[issuesKey] = append([]*gh.Issue{issue}, c.Issues[issuesKey]...)
	c.OpenIssues[issuesKey] = append([]*gh.Issue{issue}, c.OpenIssues[issuesKey]...)
	return issue, nil
}

// HoldIssueComments pauses comment reads until ReleaseIssueComments is called.
// Full-stack tests use the gate to control accepted background sync completion.
func (c *FixtureClient) HoldIssueComments() {
	c.issueCommentsGateMu.Lock()
	defer c.issueCommentsGateMu.Unlock()
	if c.issueCommentsGate == nil {
		c.issueCommentsGate = make(chan struct{})
	}
}

// ReleaseIssueComments resumes comment reads paused by HoldIssueComments.
func (c *FixtureClient) ReleaseIssueComments() {
	c.issueCommentsGateMu.Lock()
	defer c.issueCommentsGateMu.Unlock()
	if c.issueCommentsGate != nil {
		close(c.issueCommentsGate)
		c.issueCommentsGate = nil
	}
}

func (c *FixtureClient) waitForIssueComments(ctx context.Context) error {
	c.issueCommentsGateMu.Lock()
	gate := c.issueCommentsGate
	c.issueCommentsGateMu.Unlock()
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ListIssueComments returns the seeded comments for an issue or pull request.
func (c *FixtureClient) ListIssueComments(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	if err := c.waitForIssueComments(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	comments := c.Comments[issueKey(owner, repo, number)]
	if len(comments) == 0 {
		return nil, nil
	}
	return slices.Clone(comments), nil
}

func (c *FixtureClient) ListIssueCommentsIfChanged(
	ctx context.Context, owner, repo string, number int,
) ([]*gh.IssueComment, error) {
	c.mu.RLock()
	empty := len(c.Comments[issueKey(owner, repo, number)]) == 0
	c.mu.RUnlock()
	if empty {
		return nil, &gh.ErrorResponse{
			Response: &http.Response{StatusCode: http.StatusNotModified},
		}
	}
	return c.ListIssueComments(ctx, owner, repo, number)
}

func (c *FixtureClient) ListReviews(
	_ context.Context, owner, repo string, number int,
) ([]*gh.PullRequestReview, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	reviews := c.Reviews[issueKey(owner, repo, number)]
	if len(reviews) == 0 {
		return nil, nil
	}
	return slices.Clone(reviews), nil
}

func (c *FixtureClient) ListPullRequestReviewThreads(
	_ context.Context,
	owner string,
	repo string,
	number int,
) ([]platformgithub.PullRequestReviewThread, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	threads := c.ReviewThreads[issueKey(owner, repo, number)]
	if len(threads) == 0 {
		return nil, nil
	}
	return slices.Clone(threads), nil
}

// ListCommits returns nil (read-only stub).
func (c *FixtureClient) ListCommits(
	_ context.Context, _, _ string, _ int,
) ([]*gh.RepositoryCommit, error) {
	return nil, nil
}

// ListForcePushEvents returns nil (read-only stub).
func (c *FixtureClient) ListForcePushEvents(
	_ context.Context, _, _ string, _ int,
) ([]platformgithub.ForcePushEvent, error) {
	return nil, nil
}

// ListPullRequestTimelineEvents returns nil (read-only stub).
func (c *FixtureClient) ListPullRequestTimelineEvents(
	_ context.Context, _, _ string, _ int,
) ([]platformgithub.PullRequestTimelineEvent, error) {
	return nil, nil
}

// GetCombinedStatus returns a seeded combined status by repo/ref.
func (c *FixtureClient) GetCombinedStatus(
	_ context.Context, owner, repo, ref string,
) (*gh.CombinedStatus, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.CombinedStatuses[refKey(owner, repo, ref)], nil
}

// ListCheckRunsForRef returns seeded check runs by repo/ref.
func (c *FixtureClient) ListCheckRunsForRef(
	_ context.Context, owner, repo, ref string,
) ([]*gh.CheckRun, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := refKey(owner, repo, ref)
	if err := c.CheckRunErrors[key]; err != nil {
		return nil, err
	}
	runs := c.CheckRuns[key]
	if len(runs) == 0 {
		return nil, nil
	}
	return cloneCheckRuns(runs), nil
}

// SetPullRequestCheckRunError makes CI check refreshes fail for a PR head.
func (c *FixtureClient) SetPullRequestCheckRunError(
	owner, repo string,
	number int,
	err error,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	headSHA := c.pullRequestHeadSHA(owner, repo, number)
	if headSHA == "" {
		return false
	}
	if c.CheckRunErrors == nil {
		c.CheckRunErrors = make(map[string]error)
	}
	c.CheckRunErrors[refKey(owner, repo, headSHA)] = err
	return true
}

// SetPullRequestCheckRunStatus updates all seeded check runs for a PR head.
func (c *FixtureClient) SetPullRequestCheckRunStatus(
	owner, repo string,
	number int,
	status, conclusion string,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	headSHA := c.pullRequestHeadSHA(owner, repo, number)
	if headSHA == "" {
		return false
	}
	key := refKey(owner, repo, headSHA)
	runs := c.CheckRuns[key]
	if len(runs) == 0 {
		return false
	}
	updated := make([]*gh.CheckRun, 0, len(runs))
	for _, run := range runs {
		if run == nil {
			updated = append(updated, nil)
			continue
		}
		copyRun := *run
		copyRun.Status = &status
		copyRun.Conclusion = &conclusion
		updated = append(updated, &copyRun)
	}
	c.CheckRuns[key] = updated
	return true
}

// SetPullRequestCheckRuns replaces the seeded check runs for a PR head.
func (c *FixtureClient) SetPullRequestCheckRuns(
	owner, repo string,
	number int,
	runs []*gh.CheckRun,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	headSHA := c.pullRequestHeadSHA(owner, repo, number)
	if headSHA == "" {
		return false
	}
	if c.CheckRuns == nil {
		c.CheckRuns = make(map[string][]*gh.CheckRun)
	}
	c.CheckRuns[refKey(owner, repo, headSHA)] = cloneCheckRuns(runs)
	return true
}

// PullRequestHeadSHA returns the seeded PR head SHA.
func (c *FixtureClient) PullRequestHeadSHA(owner, repo string, number int) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.pullRequestHeadSHA(owner, repo, number)
}

func (c *FixtureClient) pullRequestHeadSHA(owner, repo string, number int) string {
	for _, prs := range []map[string][]*gh.PullRequest{c.PRs, c.OpenPRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() == number && pr.GetHead() != nil {
				return pr.GetHead().GetSHA()
			}
		}
	}
	return ""
}

// UpdatePullRequestSHAs updates seeded PR head/base SHAs and moves ref-based fixtures.
func (c *FixtureClient) UpdatePullRequestSHAs(
	owner, repo string,
	number int,
	headSHA, baseSHA string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()

	repoKey := repoKey(owner, repo)
	oldHeadSHA := ""
	patch := func(prs []*gh.PullRequest) []*gh.PullRequest {
		patched := slices.Clone(prs)
		for i, pr := range patched {
			if pr.GetNumber() != number {
				continue
			}
			copyPR := clonePullRequest(pr)
			if copyPR.Head == nil {
				copyPR.Head = &gh.PullRequestBranch{}
			}
			if copyPR.Base == nil {
				copyPR.Base = &gh.PullRequestBranch{}
			}
			if oldHeadSHA == "" {
				oldHeadSHA = copyPR.Head.GetSHA()
			}
			copyPR.Head.SHA = &headSHA
			copyPR.Base.SHA = &baseSHA
			patched[i] = copyPR
		}
		return patched
	}

	if c.OpenPRs != nil {
		c.OpenPRs[repoKey] = patch(c.OpenPRs[repoKey])
	}
	if c.PRs != nil {
		c.PRs[repoKey] = patch(c.PRs[repoKey])
	}

	if oldHeadSHA == "" || oldHeadSHA == headSHA {
		return
	}
	oldRefKey := refKey(owner, repo, oldHeadSHA)
	newRefKey := refKey(owner, repo, headSHA)
	if combined, ok := c.CombinedStatuses[oldRefKey]; ok {
		c.CombinedStatuses[newRefKey] = combined
	}
	if runs, ok := c.CheckRuns[oldRefKey]; ok {
		c.CheckRuns[newRefKey] = cloneCheckRuns(runs)
	}
}

func clonePullRequests(prs []*gh.PullRequest) []*gh.PullRequest {
	cloned := make([]*gh.PullRequest, 0, len(prs))
	for _, pr := range prs {
		cloned = append(cloned, clonePullRequest(pr))
	}
	return cloned
}

func clonePullRequest(pr *gh.PullRequest) *gh.PullRequest {
	return cloneFixtureValue(pr)
}

func cloneCheckRuns(runs []*gh.CheckRun) []*gh.CheckRun {
	cloned := make([]*gh.CheckRun, 0, len(runs))
	for _, run := range runs {
		cloned = append(cloned, cloneFixtureValue(run))
	}
	return cloned
}

func cloneWorkflowRuns(runs []*gh.WorkflowRun) []*gh.WorkflowRun {
	cloned := make([]*gh.WorkflowRun, 0, len(runs))
	for _, run := range runs {
		cloned = append(cloned, cloneFixtureValue(run))
	}
	return cloned
}

func cloneFixtureValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	content, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var cloned T
	if err := json.Unmarshal(content, &cloned); err != nil {
		return nil
	}
	return &cloned
}

// ListWorkflowRunsForHeadSHA returns seeded action-required workflow runs.
func (c *FixtureClient) ListWorkflowRunsForHeadSHA(
	_ context.Context, owner, repo, headSHA string,
) ([]*gh.WorkflowRun, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	runs := c.WorkflowRuns[refKey(owner, repo, headSHA)]
	if len(runs) == 0 {
		return nil, nil
	}
	return cloneWorkflowRuns(runs), nil
}

// SetWorkflowRuns seeds action-required workflow runs for a repo/head SHA.
func (c *FixtureClient) SetWorkflowRuns(owner, repo, headSHA string, runs []*gh.WorkflowRun) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.WorkflowRuns == nil {
		c.WorkflowRuns = make(map[string][]*gh.WorkflowRun)
	}
	c.WorkflowRuns[refKey(owner, repo, headSHA)] = cloneWorkflowRuns(runs)
}

// ApproveWorkflowRun returns an error (mutations not supported).
func (c *FixtureClient) ApproveWorkflowRun(
	_ context.Context, _, _ string, _ int64,
) error {
	return errFixtureReadOnly
}

func (c *FixtureClient) CreateIssueComment(
	_ context.Context, owner, repo string, number int, body string,
) (*gh.IssueComment, error) {
	return c.SeedIssueComment(owner, repo, number, body, time.Now().UTC()), nil
}

// SeedIssueComment adds a provider-side comment at a controlled timestamp.
// E2E fixtures use it to exercise sync ordering that CreateIssueComment's
// current-time timestamp cannot represent.
func (c *FixtureClient) SeedIssueComment(
	owner, repo string, number int, body string, createdAt time.Time,
) *gh.IssueComment {
	c.mu.Lock()
	defer c.mu.Unlock()

	login := "fixture-bot"
	id := c.nextID
	c.nextID++

	comment := &gh.IssueComment{
		ID:        &id,
		Body:      &body,
		CreatedAt: &gh.Timestamp{Time: createdAt.UTC()},
		User:      &gh.User{Login: &login},
	}
	key := issueKey(owner, repo, number)
	c.Comments[key] = append(c.Comments[key], comment)
	updatedAt := gh.Timestamp{Time: createdAt.UTC()}
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() != number {
				continue
			}
			pr.UpdatedAt = &updatedAt
			commentCount := pr.GetComments() + 1
			pr.Comments = &commentCount
		}
	}
	for _, issues := range []map[string][]*gh.Issue{c.OpenIssues, c.Issues} {
		for _, issue := range issues[repoKey(owner, repo)] {
			if issue.GetNumber() != number {
				continue
			}
			issue.UpdatedAt = &updatedAt
			commentCount := issue.GetComments() + 1
			issue.Comments = &commentCount
		}
	}
	return comment
}

func (c *FixtureClient) EditIssueComment(
	_ context.Context, owner, repo string, commentID int64, body string,
) (*gh.IssueComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	prefix := repoKey(owner, repo) + "#"
	for key, comments := range c.Comments {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, comment := range comments {
			if comment.GetID() == commentID {
				comment.Body = &body
				updatedAt := time.Now().UTC()
				comment.UpdatedAt = &gh.Timestamp{Time: updatedAt}
				return comment, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: comment %d", errFixtureNotFound, commentID)
}

func (c *FixtureClient) DeleteIssueComment(
	_ context.Context, owner, repo string, commentID int64,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	prefix := repoKey(owner, repo) + "#"
	for key, comments := range c.Comments {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for index, comment := range comments {
			if comment.GetID() != commentID {
				continue
			}
			c.Comments[key] = slices.Delete(comments, index, index+1)
			return nil
		}
	}
	return fmt.Errorf("%w: comment %d", errFixtureNotFound, commentID)
}

func (c *FixtureClient) CreatePullRequestReviewCommentReply(
	_ context.Context, owner, repo string, number int, body string, commentID int64,
) (*gh.PullRequestComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	login := "fixture-bot"
	now := time.Now().UTC()
	id := c.nextID
	c.nextID++
	htmlURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", owner, repo, number, id)
	return &gh.PullRequestComment{
		ID:        &id,
		Body:      &body,
		CreatedAt: &gh.Timestamp{Time: now},
		UpdatedAt: &gh.Timestamp{Time: now},
		User:      &gh.User{Login: &login},
		HTMLURL:   &htmlURL,
		InReplyTo: &commentID,
	}, nil
}

// CreateReview records an approving review so full-stack e2e tests can verify
// review mutations through the HTTP API and persisted timeline state.
func (c *FixtureClient) CreateReview(
	_ context.Context, owner, repo string, number int, event, body string,
) (*gh.PullRequestReview, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createReviewLocked(owner, repo, number, event, body)
}

func (c *FixtureClient) createReviewLocked(owner, repo string, number int, event, body string) (*gh.PullRequestReview, error) {
	if c.findPullRequest(owner, repo, number) == nil {
		return nil, fmt.Errorf("%w: pull request %s/%s#%d", errFixtureNotFound, owner, repo, number)
	}

	id := c.nextID
	c.nextID++
	now := gh.Timestamp{Time: time.Now().UTC()}
	login := "fixture-bot"
	state := strings.ToUpper(event)
	if state == "APPROVE" {
		state = "APPROVED"
	}
	nodeID := fmt.Sprintf("PRR_fixture_%d", id)
	htmlURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", owner, repo, number, id)

	review := &gh.PullRequestReview{
		ID:          &id,
		NodeID:      &nodeID,
		User:        &gh.User{Login: &login},
		Body:        &body,
		State:       &state,
		SubmittedAt: &now,
		HTMLURL:     &htmlURL,
	}
	key := issueKey(owner, repo, number)
	c.Reviews[key] = append(c.Reviews[key], review)
	return review, nil
}

func (c *FixtureClient) CreateReviewWithComments(
	_ context.Context,
	owner, repo string,
	number int,
	event string,
	body string,
	_ string,
	_ []*gh.DraftReviewComment,
) (*gh.PullRequestReview, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createReviewLocked(owner, repo, number, event, body)
}

func (c *FixtureClient) ApplyReviewSuggestions(
	_ context.Context,
	owner string,
	repo string,
	number int,
	input platform.ApplyReviewSuggestionsInput,
) (*platform.AppliedReviewSuggestions, error) {
	c.mu.RLock()
	if c.reviewSuggestionResult == nil {
		c.mu.RUnlock()
		return nil, errFixtureReadOnly
	}
	result := *c.reviewSuggestionResult
	baseSHA := c.reviewSuggestionBaseSHA
	c.mu.RUnlock()
	c.UpdatePullRequestSHAs(owner, repo, number, result.CommitSHA, baseSHA)
	return &result, nil
}

func (c *FixtureClient) DismissReview(
	_ context.Context, _, _ string, _ int, reviewID int64, _ string,
) (*gh.PullRequestReview, error) {
	return &gh.PullRequestReview{ID: &reviewID}, nil
}

func (c *FixtureClient) MarkPullRequestReadyForReview(
	_ context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pr := c.updatePullRequestDraft(owner, repo, number, false)
	if pr == nil {
		return nil, nil
	}
	if number == 6 {
		return nil, &fixtureReadyForReviewStaleStateError{
			message: fmt.Sprintf(
				"marking %s/%s#%d ready for review: graphql errors: Could not resolve to a PullRequest with the global id of 'PR_fixture_%d'.",
				owner, repo, number, number,
			),
		}
	}
	return clonePullRequest(pr), nil
}

func (c *FixtureClient) ConvertPullRequestToDraft(
	_ context.Context, owner, repo string, number int,
) (*gh.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pr := c.updatePullRequestDraft(owner, repo, number, true)
	if pr == nil {
		return nil, nil
	}
	return clonePullRequest(pr), nil
}

// MergePullRequest updates the seeded pull request unless a test configured a failure.
func (c *FixtureClient) MergePullRequest(
	_ context.Context, owner, repo string, number int, _, _, _, _ string,
) (*gh.PullRequestMergeResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mergePullRequestError != nil {
		return nil, c.mergePullRequestError
	}
	if c.mergePullRequestResult != nil {
		result := *c.mergePullRequestResult
		return &result, nil
	}

	// Mutate the PR in BOTH the OpenPRs and PRs maps — like EditPullRequest —
	// so the canonical post-merge resync's GetPullRequest sees the merged
	// state instead of the pristine seed, and advance updated_at the way the
	// real provider does so the monotonic snapshot guard accepts the resync.
	now := gh.Timestamp{Time: time.Now().UTC()}
	state := "closed"
	merged := true
	var sha string
	found := false
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() != number {
				continue
			}
			pr.State = &state
			pr.Merged = &merged
			pr.ClosedAt = &now
			pr.MergedAt = &now
			pr.UpdatedAt = &now
			if !found {
				sha = pr.GetHead().GetSHA()
				found = true
			}
		}
	}
	if !found {
		return nil, nil
	}
	msg := "merged"
	return &gh.PullRequestMergeResult{SHA: &sha, Merged: &merged, Message: &msg}, nil
}

// EditPullRequest updates seeded PR fields for E2E mutations.
// Mutates the PR in BOTH the OpenPRs and PRs maps so a follow-up
// GetPullRequest (used by sync) returns the new state instead of the
// pristine seed.
func (c *FixtureClient) EditPullRequest(
	_ context.Context, owner, repo string, number int, opts platformgithub.EditPullRequestOpts,
) (*gh.PullRequest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := gh.Timestamp{Time: time.Now().UTC()}
	var updated *gh.PullRequest
	for _, prs := range []map[string][]*gh.PullRequest{c.OpenPRs, c.PRs} {
		for _, pr := range prs[repoKey(owner, repo)] {
			if pr.GetNumber() != number {
				continue
			}
			if opts.State != nil {
				pr.State = opts.State
				if *opts.State == "closed" {
					pr.ClosedAt = &now
				} else {
					pr.ClosedAt = nil
					pr.MergedAt = nil
					merged := false
					pr.Merged = &merged
				}
			}
			if opts.Title != nil {
				pr.Title = opts.Title
			}
			if opts.Body != nil {
				pr.Body = opts.Body
			}
			pr.UpdatedAt = &now
			if updated == nil {
				updated = pr
			}
		}
	}
	return clonePullRequest(updated), nil
}

// EditIssue updates the seeded issue state for E2E mutations.
// Mutates the issue in BOTH the OpenIssues and Issues maps so a
// follow-up GetIssue (used by sync) sees the new state instead of
// the pristine seed.
func (c *FixtureClient) EditIssue(
	_ context.Context, owner, repo string, number int, state string,
) (*gh.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := gh.Timestamp{Time: time.Now().UTC()}
	var updated *gh.Issue
	for _, issues := range []map[string][]*gh.Issue{c.OpenIssues, c.Issues} {
		for _, issue := range issues[repoKey(owner, repo)] {
			if issue.GetNumber() != number {
				continue
			}
			issue.State = &state
			if state == "closed" {
				issue.ClosedAt = &now
			} else {
				issue.ClosedAt = nil
			}
			if updated == nil {
				updated = issue
			}
		}
	}
	return updated, nil
}

// EditIssueContent updates the seeded issue's title and/or body so e2e
// tests exercising body edits (checkbox toggling, etc.) see persisted
// state on the next read. Mutates BOTH OpenIssues and Issues so the
// sync path observes the edit.
func (c *FixtureClient) EditIssueContent(
	_ context.Context, owner, repo string, number int, title *string, body *string,
) (*gh.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := gh.Timestamp{Time: time.Now().UTC()}
	var updated *gh.Issue
	for _, issues := range []map[string][]*gh.Issue{c.OpenIssues, c.Issues} {
		for _, issue := range issues[repoKey(owner, repo)] {
			if issue.GetNumber() != number {
				continue
			}
			if title != nil {
				issue.Title = title
			}
			if body != nil {
				issue.Body = body
			}
			issue.UpdatedAt = &now
			if updated == nil {
				updated = issue
			}
		}
	}
	return updated, nil
}

// ListPullRequestsPage returns nil (read-only stub).
func (c *FixtureClient) ListPullRequestsPage(
	_ context.Context, _, _, _ string, _ int,
) ([]*gh.PullRequest, bool, error) {
	return nil, false, nil
}

// ListIssuesPage returns nil (read-only stub).
func (c *FixtureClient) ListIssuesPage(
	_ context.Context, _, _, _ string, _ int,
) ([]*gh.Issue, bool, error) {
	return nil, false, nil
}

// InvalidateListETagsForRepo is a no-op for the fixture client,
// which has no underlying HTTP cache.
func (c *FixtureClient) InvalidateListETagsForRepo(_, _ string, _ ...string) {}
