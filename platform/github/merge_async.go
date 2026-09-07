package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	gh "github.com/google/go-github/v89/github"
)

const (
	githubAsyncMergeAPIVersion = "2026-03-10"
	mergeAsyncInitialPollDelay = 100 * time.Millisecond
	mergeAsyncMaxPollDelay     = 2 * time.Second
	mergeAsyncMaxPolls         = 120
)

type mergeAsyncRequest struct {
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	SHA           string `json:"sha,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
	MergeAction   string `json:"merge_action"`
}

type mergeAsyncResponse struct {
	Status  string            `json:"status"`
	Details mergeAsyncDetails `json:"details"`
}

type mergeAsyncDetails struct {
	Message         string `json:"message"`
	UUID            string `json:"uuid"`
	SHA             string `json:"sha"`
	MergeMethod     string `json:"merge_method"`
	MergeAction     string `json:"merge_action"`
	ExpectedHeadSHA string `json:"expected_head_sha"`
}

func (c *Client) MergePullRequest(
	ctx context.Context, owner, repo string, number int,
	commitTitle, commitMessage, method, expectedHeadSHA string,
) (*gh.PullRequestMergeResult, error) {
	if c.platformHost != "github.com" {
		return c.mergePullRequestSync(
			ctx, owner, repo, number,
			commitTitle, commitMessage, method, expectedHeadSHA,
		)
	}
	return c.mergePullRequestAsync(
		ctx, owner, repo, number,
		commitTitle, commitMessage, method, expectedHeadSHA,
	)
}

func (c *Client) mergePullRequestSync(
	ctx context.Context, owner, repo string, number int,
	commitTitle, commitMessage, method, expectedHeadSHA string,
) (*gh.PullRequestMergeResult, error) {
	opts := &gh.PullRequestOptions{
		CommitTitle: commitTitle,
		MergeMethod: method,
		// When set, GitHub rejects the merge if the PR head moved past the
		// reviewed commit.
		SHA: expectedHeadSHA,
	}
	result, resp, err := c.writeGH().PullRequests.Merge(
		ctx, owner, repo, number, commitMessage, opts,
	)
	c.trackWriteRate(resp)
	if err != nil {
		return nil, fmt.Errorf(
			"merging %s/%s#%d: %w", owner, repo, number, err,
		)
	}
	return result, nil
}

func (c *Client) mergePullRequestAsync(
	ctx context.Context, owner, repo string, number int,
	commitTitle, commitMessage, method, expectedHeadSHA string,
) (*gh.PullRequestMergeResult, error) {
	requestPath := fmt.Sprintf(
		"repos/%s/%s/pulls/%d/merge-async", owner, repo, number,
	)
	body := mergeAsyncRequest{
		CommitTitle:   commitTitle,
		CommitMessage: commitMessage,
		SHA:           expectedHeadSHA,
		MergeMethod:   method,
		MergeAction:   "direct_merge",
	}
	result, resp, err := c.doMergeAsyncRequest(
		ctx, http.MethodPut, requestPath, body,
	)
	if err != nil {
		return nil, fmt.Errorf("merging %s/%s#%d: %w", owner, repo, number, err)
	}
	if terminal, err := mergeAsyncTerminalResult(resp, result); terminal {
		if err != nil {
			return nil, fmt.Errorf("merging %s/%s#%d: %w", owner, repo, number, err)
		}
		return result.pullRequestMergeResult(), nil
	}
	if resp == nil || resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf(
			"merging %s/%s#%d: GitHub returned pending without accepting an asynchronous merge",
			owner, repo, number,
		)
	}
	if result.Details.UUID == "" {
		return nil, fmt.Errorf(
			"merging %s/%s#%d: GitHub accepted an asynchronous merge without an operation UUID",
			owner, repo, number,
		)
	}

	pollPath := requestPath + "/" + result.Details.UUID
	for poll := range mergeAsyncMaxPolls {
		if err := waitForMergeAsyncPoll(ctx, poll); err != nil {
			return nil, fmt.Errorf("merging %s/%s#%d: %w", owner, repo, number, err)
		}
		result, resp, err = c.doMergeAsyncRequest(ctx, http.MethodGet, pollPath, nil)
		if err != nil {
			return nil, fmt.Errorf("merging %s/%s#%d: %w", owner, repo, number, err)
		}
		if terminal, err := mergeAsyncTerminalResult(resp, result); terminal {
			if err != nil {
				return nil, fmt.Errorf("merging %s/%s#%d: %w", owner, repo, number, err)
			}
			return result.pullRequestMergeResult(), nil
		}
	}

	return nil, fmt.Errorf(
		"merging %s/%s#%d: GitHub asynchronous merge did not complete after %d polls",
		owner, repo, number, mergeAsyncMaxPolls,
	)
}

func (c *Client) doMergeAsyncRequest(
	ctx context.Context,
	method string,
	requestPath string,
	body any,
) (mergeAsyncResponse, *gh.Response, error) {
	var result mergeAsyncResponse
	req, err := c.writeGH().NewRequest(
		ctx, method, requestPath, body, gh.WithVersion(githubAsyncMergeAPIVersion),
	)
	if err != nil {
		return result, nil, err
	}
	resp, requestErr := c.writeGH().BareDo(req)
	c.trackWriteRate(resp)
	if resp == nil {
		return result, nil, requestErr
	}
	if requestErr != nil {
		var accepted *gh.AcceptedError
		if errors.As(requestErr, &accepted) && resp.StatusCode == http.StatusAccepted {
			if err := json.Unmarshal(accepted.Raw, &result); err != nil {
				return result, resp, err
			}
			return result, resp, nil
		}
		var githubError *gh.ErrorResponse
		if errors.As(requestErr, &githubError) &&
			resp.StatusCode == http.StatusConflict &&
			githubError.Message == "" {
			githubError.Message = "an asynchronous merge request is already in progress"
		}
		return result, resp, requestErr
	}
	defer resp.Body.Close()
	decodeErr := json.NewDecoder(resp.Body).Decode(&result)
	if decodeErr != nil {
		return result, resp, decodeErr
	}
	return result, resp, nil
}

func mergeAsyncTerminalResult(
	resp *gh.Response,
	result mergeAsyncResponse,
) (bool, error) {
	switch result.Status {
	case "pending":
		return false, nil
	case "merged":
		return true, nil
	case "failed", "enqueued":
		message := result.Details.Message
		if message == "" {
			message = "GitHub asynchronous merge ended in state " + result.Status
		}
		return true, &gh.ErrorResponse{
			Response: mergeAsyncConflictResponse(resp),
			Message:  message,
		}
	default:
		return true, fmt.Errorf(
			"GitHub asynchronous merge returned unknown status %q", result.Status,
		)
	}
}

func mergeAsyncConflictResponse(resp *gh.Response) *http.Response {
	conflict := &http.Response{
		StatusCode: http.StatusConflict,
		Status:     http.StatusText(http.StatusConflict),
		Header:     make(http.Header),
	}
	if resp == nil || resp.Response == nil {
		return conflict
	}
	conflict.Request = resp.Request
	conflict.Header = resp.Header.Clone()
	return conflict
}

func (r mergeAsyncResponse) pullRequestMergeResult() *gh.PullRequestMergeResult {
	merged := true
	return &gh.PullRequestMergeResult{
		Merged:  &merged,
		SHA:     &r.Details.SHA,
		Message: &r.Details.Message,
	}
}

func waitForMergeAsyncPoll(ctx context.Context, poll int) error {
	delay := mergeAsyncInitialPollDelay
	for i := 0; i < poll && delay < mergeAsyncMaxPollDelay; i++ {
		delay *= 2
		if delay > mergeAsyncMaxPollDelay {
			delay = mergeAsyncMaxPollDelay
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
