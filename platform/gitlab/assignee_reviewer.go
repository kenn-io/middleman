package gitlab

import (
	"context"
	"fmt"
	"slices"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
	"go.kenn.io/forge/platform"
)

func (c *Client) SetMergeRequestAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Retained assignees may be invisible to /users search even though
	// the merge request itself reports them with IDs; seed the cache
	// from the current assignee list so only new usernames need search.
	mr, _, err := c.api.MergeRequests.GetMergeRequest(pid, int64(number), nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, c.mapGitLabError("assignee_mutation", err)
	}
	for _, assignee := range mr.Assignees {
		if assignee != nil {
			c.cacheUserID(assignee.Username, assignee.ID)
		}
	}
	ids, err := c.resolveUserIDs(ctx, usernames)
	if err != nil {
		return nil, err
	}
	updated, _, err := c.api.MergeRequests.UpdateMergeRequest(
		pid,
		int64(number),
		&gitlab.UpdateMergeRequestOptions{AssigneeIDs: &ids},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, c.mapGitLabError("assignee_mutation", err)
	}
	return basicUsernames(updated.Assignees), nil
}

func (c *Client) SetIssueAssignees(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Same retained-assignee seeding as the merge request path: the
	// issue's own assignee list is the authoritative ID source.
	current, _, err := c.api.Issues.GetIssue(pid, int64(number), nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, c.mapGitLabError("assignee_mutation", err)
	}
	for _, assignee := range current.Assignees {
		if assignee != nil {
			c.cacheUserID(assignee.Username, assignee.ID)
		}
	}
	ids, err := c.resolveUserIDs(ctx, usernames)
	if err != nil {
		return nil, err
	}
	issue, _, err := c.api.Issues.UpdateIssue(
		pid,
		int64(number),
		&gitlab.UpdateIssueOptions{AssigneeIDs: &ids},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, c.mapGitLabError("assignee_mutation", err)
	}
	assignees := make([]string, 0, len(issue.Assignees))
	for _, a := range issue.Assignees {
		if a != nil && a.Username != "" {
			assignees = append(assignees, a.Username)
		}
	}
	return assignees, nil
}

// RequestMergeRequestReviewers adds the given users to the merge request
// reviewer set. GitLab models reviewers as a replace-set field, so the
// current set is read first and the union written back.
func (c *Client) RequestMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return c.mutateMergeRequestReviewers(ctx, ref, number, func(current []string) []string {
		next := slices.Clone(current)
		for _, username := range usernames {
			if !containsFold(next, username) {
				next = append(next, username)
			}
		}
		return next
	})
}

// RemoveMergeRequestReviewers removes the given users from the merge
// request reviewer set.
func (c *Client) RemoveMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	usernames []string,
) ([]string, error) {
	return c.mutateMergeRequestReviewers(ctx, ref, number, func(current []string) []string {
		next := make([]string, 0, len(current))
		for _, reviewer := range current {
			if !containsFold(usernames, reviewer) {
				next = append(next, reviewer)
			}
		}
		return next
	})
}

func (c *Client) mutateMergeRequestReviewers(
	ctx context.Context,
	ref platform.RepoRef,
	number int,
	apply func(current []string) []string,
) ([]string, error) {
	pid, _, err := c.projectScopedArg(ctx, ref)
	if err != nil {
		return nil, err
	}
	mr, _, err := c.api.MergeRequests.GetMergeRequest(pid, int64(number), nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, c.mapGitLabError("reviewer_mutation", err)
	}
	current := basicUsernames(mr.Reviewers)
	// The merge request already carries the IDs of its current
	// reviewers; seed the cache with them so retained reviewers never
	// depend on /users search, which may not return every user the
	// caller can already see on the merge request.
	for _, reviewer := range mr.Reviewers {
		if reviewer != nil && reviewer.Username != "" {
			c.cacheUserID(reviewer.Username, reviewer.ID)
		}
	}
	next := apply(current)
	if slices.Equal(next, current) {
		return current, nil
	}
	ids, err := c.resolveUserIDs(ctx, next)
	if err != nil {
		return nil, err
	}
	updated, _, err := c.api.MergeRequests.UpdateMergeRequest(
		pid,
		int64(number),
		&gitlab.UpdateMergeRequestOptions{ReviewerIDs: &ids},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return nil, c.mapGitLabError("reviewer_mutation", err)
	}
	return basicUsernames(updated.Reviewers), nil
}

// resolveUserIDs maps usernames to GitLab user IDs, consulting the
// client-lifetime cache before issuing exact-username lookups. The
// returned slice is never nil so an empty set serializes as [] and
// clears the provider-side assignment.
func (c *Client) resolveUserIDs(ctx context.Context, usernames []string) ([]int64, error) {
	ids := make([]int64, 0, len(usernames))
	for _, username := range usernames {
		id, err := c.lookupUserID(ctx, username)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *Client) cacheUserID(username string, id int64) {
	key := strings.ToLower(strings.TrimSpace(username))
	if key == "" || id == 0 {
		return
	}
	c.userIDMu.Lock()
	if c.userIDs == nil {
		c.userIDs = make(map[string]int64)
	}
	c.userIDs[key] = id
	c.userIDMu.Unlock()
}

func (c *Client) lookupUserID(ctx context.Context, username string) (int64, error) {
	key := strings.ToLower(strings.TrimSpace(username))
	c.userIDMu.Lock()
	id, ok := c.userIDs[key]
	c.userIDMu.Unlock()
	if ok {
		return id, nil
	}

	users, _, err := c.api.Users.ListUsers(
		&gitlab.ListUsersOptions{Username: &username},
		gitlab.WithContext(ctx),
	)
	if err != nil {
		return 0, c.mapGitLabError("user_lookup", err)
	}
	for _, user := range users {
		if user == nil || !strings.EqualFold(user.Username, username) {
			continue
		}
		c.cacheUserID(user.Username, user.ID)
		return user.ID, nil
	}
	return 0, &platform.Error{
		Code:         platform.ErrCodeNotFound,
		Provider:     platform.KindGitLab,
		PlatformHost: c.host,
		Capability:   "user_lookup",
		Err:          fmt.Errorf("gitlab user %q not found", username),
	}
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}
