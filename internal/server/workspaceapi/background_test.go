package workspaceapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/platform"
)

func TestPublishWorkspacePRAssociationUpdatesBroadcastsInvalidation(t *testing.T) {
	var events []Event
	h := New(Deps{Broadcast: func(event Event) uint64 {
		events = append(events, event)
		return uint64(len(events))
	}})

	h.publishWorkspacePRAssociationUpdates([]workspace.PRAssociationUpdate{{
		WorkspaceID: "ws-1",
		PRNumber:    42,
	}})

	require.Len(t, events, 2)
	assert.Equal(t, Event{Type: "workspace_status", Data: map[string]string{"id": "ws-1"}}, events[0])
	assert.Equal(t, Event{Type: "data_changed", Data: struct{}{}}, events[1])
}

func TestPublishWorkspacePushedHeadResultBroadcastsDomainEvents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var events []Event
	h := New(Deps{Broadcast: func(event Event) uint64 {
		events = append(events, event)
		return uint64(len(events))
	}})
	observedAt := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	h.publishWorkspacePushedHeadResult(workspace.PushedHeadPassResult{
		Associations: []workspace.WorkspacePRAssociation{{
			WorkspaceID: "ws-issue", Provider: platform.KindGitHub,
			PlatformHost: "github.com", RepoPath: "acme/widget",
			Owner: "acme", Name: "widget", IssueNumber: 7, PRNumber: 42,
			AssociatedAt: observedAt,
		}},
		HeadChanges: []workspace.PushedHeadUpdate{{
			WorkspaceID: "ws-pr", RepoID: 1, Provider: platform.KindGitHub,
			PlatformHost: "github.com", RepoPath: "acme/widget",
			Owner: "acme", Name: "widget", Number: 42,
			OldSHA: "old", NewSHA: "new", RemoteName: "origin",
			BranchName: "feature", TrackingRef: "refs/remotes/origin/feature",
			ObservedAt: observedAt,
		}},
	})

	require.Len(events, 4)
	assert.Equal("workspace_pr_associated", events[0].Type)
	assert.Equal("workspace_status", events[1].Type)
	assert.Equal("data_changed", events[2].Type)
	assert.Equal("workspace_pushed_head_changed", events[3].Type)
}
