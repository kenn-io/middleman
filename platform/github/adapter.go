package github

import (
	"errors"
	"time"
)

var (
	ErrNilPullRequest = errors.New("nil pull request")
	ErrNilIssue       = errors.New("nil issue")
)

type ForcePushEvent struct {
	Actor     string
	BeforeSHA string
	AfterSHA  string
	Ref       string
	CreatedAt time.Time
}

type PullRequestTimelineEvent struct {
	NodeID               string
	EventType            string
	Actor                string
	Assignee             string
	CreatedAt            time.Time
	DeletedCommentAuthor string
	BeforeSHA            string
	AfterSHA             string
	Ref                  string
	PreviousTitle        string
	CurrentTitle         string
	PreviousRefName      string
	CurrentRefName       string
	SourceType           string
	SourceOwner          string
	SourceRepo           string
	SourceNumber         int
	SourceTitle          string
	SourceURL            string
	IsCrossRepository    bool
	WillCloseTarget      bool
}
