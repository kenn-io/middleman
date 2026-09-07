package github

import (
	"testing"

	gh "github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/assert"
	platformgithub "go.kenn.io/forge/platform/github"
)

func TestFilterWorkflowRunsAwaitingApproval(t *testing.T) {
	tests := []struct {
		name    string
		runs    []*gh.WorkflowRun
		pr      PRSource
		wantIDs []int64
	}{
		{
			name: "matches pull request event head sha and number",
			pr: PRSource{
				Number:           42,
				HeadSHA:          "abc123",
				HeadRepoFullName: "acme/widget",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				ghWorkflowRun(101, "abc123", withWorkflowPRNumbers(42)),
				ghWorkflowRun(103, "def456"),
			},
			wantIDs: []int64{101},
		},
		{
			name: "includes fork PR runs with empty PullRequests when head repo and ref match",
			pr: PRSource{
				Number:           7,
				HeadSHA:          "abc123",
				HeadRepoFullName: "fork/widget",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				ghWorkflowRun(201, "abc123", withWorkflowHeadBranch("feature"), withWorkflowHeadRepo("fork/widget")),
				ghWorkflowRun(202, "abc123", withWorkflowHeadBranch("feature"), withWorkflowHeadRepo("fork/widget"), withWorkflowPRNumbers()),
			},
			wantIDs: []int64{201, 202},
		},
		{
			name: "rejects populated PullRequests pointing at another PR at same SHA",
			pr: PRSource{
				Number:           42,
				HeadSHA:          "abc123",
				HeadRepoFullName: "acme/widget",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				ghWorkflowRun(301, "abc123", withWorkflowPRNumbers(42)),
				ghWorkflowRun(302, "abc123", withWorkflowPRNumbers(99)),
			},
			wantIDs: []int64{301},
		},
		{
			name: "rejects fork PR run from a different fork at same head SHA",
			pr: PRSource{
				Number:           7,
				HeadSHA:          "abc123",
				HeadRepoFullName: "alice/widget",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				{
					ID:             new(int64(401)),
					HeadSHA:        new("abc123"),
					Event:          new("pull_request"),
					HeadBranch:     new("feature"),
					HeadRepository: &gh.Repository{FullName: new("alice/widget")},
				},
				{
					ID:             new(int64(402)),
					HeadSHA:        new("abc123"),
					Event:          new("pull_request"),
					HeadBranch:     new("feature"),
					HeadRepository: &gh.Repository{FullName: new("bob/widget")},
				},
			},
			wantIDs: []int64{401},
		},
		{
			name: "rejects fork PR run from same fork but different branch at same SHA",
			pr: PRSource{
				Number:           7,
				HeadSHA:          "abc123",
				HeadRepoFullName: "alice/widget",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				{
					ID:             new(int64(501)),
					HeadSHA:        new("abc123"),
					Event:          new("pull_request"),
					HeadBranch:     new("other-branch"),
					HeadRepository: &gh.Repository{FullName: new("alice/widget")},
				},
			},
			wantIDs: []int64{},
		},
		{
			name: "fails closed when head repo full name unknown and PullRequests empty",
			pr: PRSource{
				Number:           7,
				HeadSHA:          "abc123",
				HeadRepoFullName: "",
				HeadRef:          "feature",
			},
			runs: []*gh.WorkflowRun{
				{
					ID:             new(int64(601)),
					HeadSHA:        new("abc123"),
					Event:          new("pull_request"),
					HeadBranch:     new("feature"),
					HeadRepository: &gh.Repository{FullName: new("fork/widget")},
				},
			},
			wantIDs: []int64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterWorkflowRunsAwaitingApproval(tt.runs, tt.pr)
			gotIDs := make([]int64, 0, len(got))
			for _, run := range got {
				gotIDs = append(gotIDs, run.GetID())
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestParseHeadRepoFullName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "https with .git", in: "https://github.com/cwensel/roborev.git", want: "cwensel/roborev"},
		{name: "https without .git", in: "https://github.com/cwensel/roborev", want: "cwensel/roborev"},
		{name: "ssh form", in: "git@github.com:cwensel/roborev.git", want: "cwensel/roborev"},
		{name: "trailing slash", in: "https://github.com/cwensel/roborev/", want: "cwensel/roborev"},
		{name: "empty", in: "", want: ""},
		{name: "garbage", in: "not-a-url", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, platformgithub.ParseHeadRepoFullName(tt.in))
		})
	}
}

func TestWorkflowApprovalStateFromRuns(t *testing.T) {
	assert := assert.New(t)
	runs := []*gh.WorkflowRun{
		{ID: new(int64(11))},
		{ID: new(int64(12))},
	}

	got := WorkflowApprovalStateFromRuns(runs)

	assert.True(got.Checked)
	assert.True(got.Required)
	assert.Equal(2, got.Count)
	assert.Equal([]int64{11, 12}, got.RunIDs)
}
