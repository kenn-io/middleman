package gitealike

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/forge/platform"
)

const maxTimelineResponseBytes = 16 << 20

type timelineUserResponse struct {
	ID       int64  `json:"id"`
	UserName string `json:"login"`
	FullName string `json:"full_name"`
}

type timelineRepositoryResponse struct {
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	FullName string `json:"full_name"`
}

type timelineIssueResponse struct {
	Number      int                         `json:"number"`
	Title       string                      `json:"title"`
	HTMLURL     string                      `json:"html_url"`
	PullRequest *struct{}                   `json:"pull_request"`
	Repository  *timelineRepositoryResponse `json:"repository"`
}

type timelineEventResponse struct {
	ID            int64                  `json:"id"`
	HTMLURL       string                 `json:"html_url"`
	User          timelineUserResponse   `json:"user"`
	Type          string                 `json:"type"`
	Body          string                 `json:"body"`
	Assignee      timelineUserResponse   `json:"assignee"`
	PreviousTitle string                 `json:"old_title"`
	CurrentTitle  string                 `json:"new_title"`
	Reference     *timelineIssueResponse `json:"ref_issue"`
	RefAction     string                 `json:"ref_action"`
	Created       time.Time              `json:"created_at"`
	Updated       time.Time              `json:"updated_at"`
}

func ReadIssueTimelinePage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	ref platform.RepoRef,
	number int,
	opts PageOptions,
) ([]TimelineEventDTO, Page, error) {
	endpoint, err := url.JoinPath(
		baseURL, "api", "v1", "repos", ref.Owner, ref.Name,
		"issues", strconv.Itoa(number), "timeline",
	)
	if err != nil {
		return nil, Page{}, fmt.Errorf("build issue timeline URL: %w", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, Page{}, fmt.Errorf("parse issue timeline URL: %w", err)
	}
	query := parsed.Query()
	query.Set("page", strconv.Itoa(opts.Page))
	query.Set("limit", strconv.Itoa(opts.PageSize))
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, Page{}, fmt.Errorf("build issue timeline request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, Page{}, fmt.Errorf("read issue timeline: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTimelineResponseBytes))
		return nil, Page{}, &HTTPError{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("read issue timeline: %s", resp.Status),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTimelineResponseBytes+1))
	if err != nil {
		return nil, Page{}, fmt.Errorf("read issue timeline response: %w", err)
	}
	if len(body) > maxTimelineResponseBytes {
		return nil, Page{}, fmt.Errorf("read issue timeline response: response exceeds %d bytes", maxTimelineResponseBytes)
	}
	var raw []timelineEventResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, Page{}, fmt.Errorf("decode issue timeline response: %w", err)
	}

	events := make([]TimelineEventDTO, 0, len(raw))
	for i := range raw {
		events = append(events, normalizeTimelineResponse(raw[i]))
	}
	return events, timelinePageFromLink(resp.Header.Get("Link")), nil
}

func normalizeTimelineResponse(raw timelineEventResponse) TimelineEventDTO {
	event := TimelineEventDTO{
		ID:            raw.ID,
		HTMLURL:       raw.HTMLURL,
		User:          UserDTO{ID: raw.User.ID, UserName: raw.User.UserName, FullName: raw.User.FullName},
		Type:          raw.Type,
		Body:          raw.Body,
		Assignee:      UserDTO{ID: raw.Assignee.ID, UserName: raw.Assignee.UserName, FullName: raw.Assignee.FullName},
		PreviousTitle: raw.PreviousTitle,
		CurrentTitle:  raw.CurrentTitle,
		RefAction:     raw.RefAction,
		Created:       raw.Created,
		Updated:       raw.Updated,
	}
	if raw.Reference == nil || raw.Reference.Repository == nil {
		return event
	}
	repoPath := strings.Trim(raw.Reference.Repository.FullName, "/")
	owner, repo := path.Split(repoPath)
	owner = strings.TrimSuffix(owner, "/")
	if owner == "" || repo == "" {
		owner = strings.TrimSpace(raw.Reference.Repository.Owner)
		repo = strings.TrimSpace(raw.Reference.Repository.Name)
	}
	event.Reference = &IssueReferenceDTO{
		Owner:         owner,
		Repo:          repo,
		Number:        raw.Reference.Number,
		Title:         raw.Reference.Title,
		HTMLURL:       raw.Reference.HTMLURL,
		IsPullRequest: raw.Reference.PullRequest != nil,
	}
	return event
}

func timelinePageFromLink(link string) Page {
	var page Page
	for entry := range strings.SplitSeq(link, ",") {
		rawURL, rawRel, ok := strings.Cut(entry, ";")
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(rawRel), "=")
		if !ok || key != "rel" {
			continue
		}
		parsed, err := url.Parse(strings.Trim(rawURL, " <>"))
		if err != nil {
			continue
		}
		number, err := strconv.Atoi(parsed.Query().Get("page"))
		if err != nil {
			continue
		}
		switch strings.Trim(value, `"`) {
		case "next":
			page.Next = number
		case "last":
			page.Last = number
		}
	}
	return page
}
