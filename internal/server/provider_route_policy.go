package server

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"

	"go.kenn.io/forge/internal/federationauth"
)

// RouteOwner identifies the daemon that owns an API operation's domain data.
type RouteOwner uint8

const (
	ProviderHubOnly RouteOwner = iota
	ProviderWithLocalOverlay
	NodeLocal
)

// ProviderRouteRule keeps domain ownership separate from federation
// authorization. Empty PeerScope means the route is local-user-only.
type ProviderRouteRule struct {
	OperationID string
	Owner       RouteOwner
	PeerScope   federationauth.Scope
}

// This table is intentionally exhaustive. A newly registered operation must
// make an explicit ownership decision before the coverage gate passes.
var providerRouteDeclarations = []ProviderRouteRule{
	{OperationID: "activate-federation-enrollment", Owner: NodeLocal},
	{OperationID: "add-repo", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "abort-federation-enrollment", Owner: NodeLocal},
	{OperationID: "abort-federation-spoke-preparation", Owner: NodeLocal},
	{OperationID: "apply-pr-review-suggestions", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "apply-pr-review-suggestions-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "approve-pull", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "approve-pull-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "approve-pull-workflows", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "approve-pull-workflows-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "begin-federation-enrollment", Owner: NodeLocal},
	{OperationID: "begin-federation-spoke-preparation", Owner: NodeLocal},
	{OperationID: "browse-docs-folders", Owner: NodeLocal},
	{OperationID: "bulk-add-repos", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "capture-telemetry-event", Owner: NodeLocal},
	{OperationID: "clone-fleet-project", Owner: NodeLocal},
	{OperationID: "clone-project", Owner: NodeLocal},
	{OperationID: "complete-filesystem-path", Owner: NodeLocal},
	{OperationID: "complete-fleet-filesystem-path", Owner: NodeLocal},
	{OperationID: "create-docs-file", Owner: NodeLocal},
	{OperationID: "create-docs-folder", Owner: NodeLocal},
	{OperationID: "create-fleet-enrollment-token", Owner: NodeLocal},
	{OperationID: "create-fleet-issue-workspace", Owner: NodeLocal},
	{OperationID: "create-fleet-issue-workspace-on-platform-host", Owner: NodeLocal},
	{OperationID: "create-fleet-repo-workspace", Owner: NodeLocal},
	{OperationID: "create-fleet-repo-workspace-on-platform-host", Owner: NodeLocal},
	{OperationID: "create-fleet-project-worktree", Owner: NodeLocal},
	{OperationID: "create-fleet-project-worktree-from-merge-request", Owner: NodeLocal},
	{OperationID: "create-fleet-workspace", Owner: NodeLocal},
	{OperationID: "create-issue", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "create-issue-kata-link", Owner: NodeLocal},
	{OperationID: "create-issue-kata-link-on-host", Owner: NodeLocal},
	{OperationID: "create-issue-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "create-issue-workspace", Owner: NodeLocal},
	{OperationID: "create-issue-workspace-on-host", Owner: NodeLocal},
	{OperationID: "create-kata-workspace", Owner: NodeLocal},
	{OperationID: "create-pr-review-draft-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "create-pr-review-draft-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "create-pull-request-kata-link", Owner: NodeLocal},
	{OperationID: "create-pull-request-kata-link-on-host", Owner: NodeLocal},
	{OperationID: "create-repo-preset", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "create-repo-workspace", Owner: NodeLocal},
	{OperationID: "create-repo-workspace-on-host", Owner: NodeLocal},
	{OperationID: "create-workspace", Owner: NodeLocal},
	{OperationID: "create-workspace-kata-link", Owner: NodeLocal},
	{OperationID: "create-worktree-from-merge-request", Owner: NodeLocal},
	{OperationID: "defer-merge-pull", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "defer-merge-pull-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-docs-file", Owner: NodeLocal},
	{OperationID: "delete-docs-folder", Owner: NodeLocal},
	{OperationID: "delete-fleet-project", Owner: NodeLocal},
	{OperationID: "delete-fleet-workspace", Owner: NodeLocal},
	{OperationID: "delete-issue-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-issue-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-issue-kata-link", Owner: NodeLocal},
	{OperationID: "delete-issue-kata-link-on-host", Owner: NodeLocal},
	{OperationID: "delete-pr-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-pr-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-pr-review-draft-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-pr-review-draft-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-project", Owner: NodeLocal},
	{OperationID: "delete-pull-request-kata-link", Owner: NodeLocal},
	{OperationID: "delete-pull-request-kata-link-on-host", Owner: NodeLocal},
	{OperationID: "delete-repo", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-repo-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-repo-preset", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "delete-workspace", Owner: NodeLocal},
	{OperationID: "delete-workspace-kata-link", Owner: NodeLocal},
	{OperationID: "delete-worktree", Owner: NodeLocal},
	{OperationID: "discard-pr-review-draft", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "discard-pr-review-draft-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-issue-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-issue-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-issue-content", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-issue-content-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-content", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-content-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-review-draft-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "edit-pr-review-draft-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "enqueue-issue-sync", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "enqueue-issue-sync-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "enqueue-pr-sync", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "enqueue-pr-sync-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "ensure-fleet-project-worktree-runtime-shell", Owner: NodeLocal},
	{OperationID: "ensure-project-worktree-runtime-shell", Owner: NodeLocal},
	{OperationID: "federation-auto-assign-workspace-item", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "federation-filter-unassigned-activity-subjects", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-get-diff-descriptor", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-get-provider-settings", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-get-repository-descriptor", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-import-review-draft", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderHandoff},
	{OperationID: "federation-import-workflow-state", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderHandoff},
	{OperationID: "federation-list-workflow-states", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-refresh-workspace-launch-spec", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "federation-resolve-workspace-launch-spec", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "federation-set-workflow-state", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "federation-update-provider-settings", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "get-archive-report", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-comment-autocomplete", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-comment-autocomplete-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-docs-git-changes", Owner: NodeLocal},
	{OperationID: "get-docs-git-status", Owner: NodeLocal},
	{OperationID: "get-docs-tree", Owner: NodeLocal},
	{OperationID: "get-federation-identity", Owner: NodeLocal},
	{OperationID: "get-fleet-host-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-fleet-project", Owner: NodeLocal},
	{OperationID: "get-fleet-project-worktree-runtime", Owner: NodeLocal},
	{OperationID: "get-fleet-project-worktree-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-fleet-settings", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-commits", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-diff", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-file-preview", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-files", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-runtime", Owner: NodeLocal},
	{OperationID: "get-fleet-workspace-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-host-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-issue", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-issue-on-host", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-kata-issue-detail", Owner: NodeLocal},
	{OperationID: "get-kata-launch-target", Owner: NodeLocal},
	{OperationID: "get-kata-project-mappings", Owner: NodeLocal},
	{OperationID: "get-markdown-image", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-markdown-image-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pr-review-draft", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pr-review-draft-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-project", Owner: NodeLocal},
	{OperationID: "get-project-worktree-runtime", Owner: NodeLocal},
	{OperationID: "get-project-worktree-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-pull", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pull-commits", Owner: NodeLocal},
	{OperationID: "get-pull-commits-on-host", Owner: NodeLocal},
	{OperationID: "get-pull-diff", Owner: NodeLocal},
	{OperationID: "get-pull-diff-on-host", Owner: NodeLocal},
	{OperationID: "get-pull-file-preview", Owner: NodeLocal},
	{OperationID: "get-pull-file-preview-on-host", Owner: NodeLocal},
	{OperationID: "get-pull-files", Owner: NodeLocal},
	{OperationID: "get-pull-files-on-host", Owner: NodeLocal},
	{OperationID: "get-pull-import-metadata", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pull-import-metadata-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pull-on-host", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pull-stack", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-pull-stack-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-rate-limits", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-repo", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-repo-browser-asset", Owner: NodeLocal},
	{OperationID: "get-repo-browser-asset-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-browser-blob", Owner: NodeLocal},
	{OperationID: "get-repo-browser-blob-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-browser-commit", Owner: NodeLocal},
	{OperationID: "get-repo-browser-commit-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-browser-history", Owner: NodeLocal},
	{OperationID: "get-repo-browser-history-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-browser-last-changed", Owner: NodeLocal},
	{OperationID: "get-repo-browser-last-changed-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-commit-diff", Owner: NodeLocal},
	{OperationID: "get-repo-commit-diff-on-host", Owner: NodeLocal},
	{OperationID: "get-repo-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-roborev-status", Owner: NodeLocal},
	{OperationID: "get-settings", Owner: NodeLocal},
	{OperationID: "get-snapshot", Owner: NodeLocal},
	{OperationID: "get-snapshot-aggregate", Owner: NodeLocal},
	{OperationID: "get-snapshot-raw", Owner: NodeLocal},
	{OperationID: "get-sync-status", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "get-tooling-status", Owner: NodeLocal},
	{OperationID: "get-version", Owner: NodeLocal},
	{OperationID: "get-workspace", Owner: NodeLocal},
	{OperationID: "get-workspace-commits", Owner: NodeLocal},
	{OperationID: "get-workspace-diff", Owner: NodeLocal},
	{OperationID: "get-workspace-file-preview", Owner: NodeLocal},
	{OperationID: "get-workspace-files", Owner: NodeLocal},
	{OperationID: "get-workspace-runtime", Owner: NodeLocal},
	{OperationID: "get-workspace-runtime-session-attach-spec", Owner: NodeLocal},
	{OperationID: "get-workspace-runtime-session-initial-message", Owner: NodeLocal},
	{OperationID: "inspect-fleet-project-worktree", Owner: NodeLocal},
	{OperationID: "inspect-project-worktree", Owner: NodeLocal},
	{OperationID: "join-federation", Owner: NodeLocal},
	{OperationID: "launch-fleet-host-runtime-session", Owner: NodeLocal},
	{OperationID: "launch-fleet-project-worktree-runtime-session", Owner: NodeLocal},
	{OperationID: "launch-fleet-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "launch-host-runtime-session", Owner: NodeLocal},
	{OperationID: "launch-project-worktree-runtime-session", Owner: NodeLocal},
	{OperationID: "launch-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "list-activity", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-activity-authors", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-activity-thread-events", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-archive-pacing", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-archive-status", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-docs-folders", Owner: NodeLocal},
	{OperationID: "list-fleet-project-branches", Owner: NodeLocal},
	{OperationID: "list-fleet-project-worktrees", Owner: NodeLocal},
	{OperationID: "list-fleet-workspaces", Owner: NodeLocal},
	{OperationID: "list-host-runtime-sessions", Owner: NodeLocal},
	{OperationID: "list-issue-kata-links", Owner: NodeLocal},
	{OperationID: "list-issue-kata-links-on-host", Owner: NodeLocal},
	{OperationID: "list-issues", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-kata-daemons", Owner: NodeLocal},
	{OperationID: "list-kata-references", Owner: NodeLocal},
	{OperationID: "list-launch-targets", Owner: NodeLocal},
	{OperationID: "list-notifications", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-project-branches", Owner: NodeLocal},
	{OperationID: "list-projects", Owner: NodeLocal},
	{OperationID: "list-pull-request-kata-links", Owner: NodeLocal},
	{OperationID: "list-pull-request-kata-links-on-host", Owner: NodeLocal},
	{OperationID: "list-pulls", Owner: ProviderWithLocalOverlay, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-repo-browser-refs", Owner: NodeLocal},
	{OperationID: "list-repo-browser-refs-on-host", Owner: NodeLocal},
	{OperationID: "list-repo-browser-tree", Owner: NodeLocal},
	{OperationID: "list-repo-browser-tree-on-host", Owner: NodeLocal},
	{OperationID: "list-repo-labels", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-repo-labels-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-repo-summaries", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-repos", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-roborev-configured-repositories", Owner: NodeLocal},
	{OperationID: "list-stacks", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "list-user-repositories", Owner: NodeLocal},
	{OperationID: "list-workspace-agent-sessions", Owner: NodeLocal},
	{OperationID: "list-workspace-kata-links", Owner: NodeLocal},
	{OperationID: "list-workspaces", Owner: NodeLocal},
	{OperationID: "list-worktrees", Owner: NodeLocal},
	{OperationID: "mark-notifications-done", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "mark-notifications-read", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "mark-notifications-undone", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "mark-pull-ready-for-review", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "mark-pull-ready-for-review-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "merge-pull", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "merge-pull-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "pause-archives", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "post-issue-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "post-issue-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "post-pr-comment", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "post-pr-comment-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "prepare-federation-spoke", Owner: NodeLocal},
	{OperationID: "preview-repos", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "publish-docs-git", Owner: NodeLocal},
	{OperationID: "publish-pr-review-draft", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "publish-pr-review-draft-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "pull-docs-git", Owner: NodeLocal},
	{OperationID: "pull-fleet-workspace-branch", Owner: NodeLocal},
	{OperationID: "pull-workspace-branch", Owner: NodeLocal},
	{OperationID: "push-fleet-workspace-branch", Owner: NodeLocal},
	{OperationID: "push-workspace-branch", Owner: NodeLocal},
	{OperationID: "read-docs-blob", Owner: NodeLocal},
	{OperationID: "read-docs-file", Owner: NodeLocal},
	{OperationID: "receive-agent-hook", Owner: NodeLocal},
	{OperationID: "refresh-fleet-project-worktree-stats", Owner: NodeLocal},
	{OperationID: "refresh-fleet-stats", Owner: NodeLocal},
	{OperationID: "refresh-fleet-workspace", Owner: NodeLocal},
	{OperationID: "refresh-pull-ci", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "refresh-pull-ci-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "refresh-repo", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "refresh-repo-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "refresh-workspace", Owner: NodeLocal},
	{OperationID: "refresh-worktree-stats", Owner: NodeLocal},
	{OperationID: "register-fleet-project", Owner: NodeLocal},
	{OperationID: "register-project", Owner: NodeLocal},
	{OperationID: "register-worktree", Owner: NodeLocal},
	{OperationID: "remove-fleet-project-worktree", Owner: NodeLocal},
	{OperationID: "remove-stale-worktree", Owner: NodeLocal},
	{OperationID: "remove-worktree", Owner: NodeLocal},
	{OperationID: "rename-docs-file", Owner: NodeLocal},
	{OperationID: "rename-fleet-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "rename-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "reply-to-discussion", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "reply-to-discussion-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "request-pull-changes", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "request-pull-changes-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "resolve-discussion", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "resolve-discussion-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "resolve-kata-issue-reference", Owner: NodeLocal},
	{OperationID: "resolve-pr-review-thread", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "resolve-pr-review-thread-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "resolve-repo-item", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "resolve-repo-item-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderRead},
	{OperationID: "retry-fleet-workspace", Owner: NodeLocal},
	{OperationID: "retry-workspace", Owner: NodeLocal},
	{OperationID: "reveal-fleet-workspace", Owner: NodeLocal},
	{OperationID: "reveal-workspace", Owner: NodeLocal},
	{OperationID: "revoke-federation-enrollment", Owner: NodeLocal},
	{OperationID: "search-docs", Owner: NodeLocal},
	{OperationID: "search-docs-folder", Owner: NodeLocal},
	{OperationID: "set-active-worktree", Owner: NodeLocal},
	{OperationID: "set-fleet-project-worktree-links", Owner: NodeLocal},
	{OperationID: "set-fleet-project-worktree-session-backend", Owner: NodeLocal},
	{OperationID: "set-issue-assignees", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-issue-assignees-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-issue-github-state", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-issue-github-state-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-issue-labels", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-issue-labels-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-kanban-state", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-kanban-state-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-assignees", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-assignees-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-github-state", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-github-state-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-labels", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-labels-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-reviewers", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-pr-reviewers-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-starred", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "set-worktree-hidden", Owner: NodeLocal},
	{OperationID: "set-worktree-links", Owner: NodeLocal},
	{OperationID: "set-worktree-session-backend", Owner: NodeLocal},
	{OperationID: "seal-federation-spoke-preparation", Owner: NodeLocal},
	{OperationID: "start-archives", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "stop-fleet-host-runtime-session", Owner: NodeLocal},
	{OperationID: "stop-fleet-project-worktree-runtime-session", Owner: NodeLocal},
	{OperationID: "stop-fleet-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "stop-host-runtime-session", Owner: NodeLocal},
	{OperationID: "stop-project-worktree-runtime-session", Owner: NodeLocal},
	{OperationID: "stop-workspace-runtime-session", Owner: NodeLocal},
	{OperationID: "stream-events", Owner: NodeLocal},
	{OperationID: "stream-federation-provider-events", Owner: NodeLocal},
	{OperationID: "submit-workspace-runtime-session-initial-message", Owner: NodeLocal},
	{OperationID: "sync-issue", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "sync-issue-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "sync-notifications", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "sync-pull", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "sync-pull-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "trigger-sync", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "unresolve-pr-review-thread", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "unresolve-pr-review-thread-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "unset-starred", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "update-docs-folder", Owner: NodeLocal},
	{OperationID: "update-fleet-settings", Owner: NodeLocal},
	{OperationID: "update-repo-preset", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "update-repo-ui-visibility", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "update-repo-ui-visibility-on-host", Owner: ProviderHubOnly, PeerScope: federationauth.ScopeProviderWrite},
	{OperationID: "update-repo-worktree-base", Owner: NodeLocal},
	{OperationID: "update-repo-worktree-base-on-host", Owner: NodeLocal},
	{OperationID: "update-settings", Owner: NodeLocal},
	{OperationID: "validate-filesystem-repo", Owner: NodeLocal},
	{OperationID: "validate-fleet-filesystem-repo", Owner: NodeLocal},
	{OperationID: "watch-fleet-workspace-diff", Owner: NodeLocal},
	{OperationID: "watch-workspace-diff", Owner: NodeLocal},
	{OperationID: "write-docs-file", Owner: NodeLocal},
}

func providerRouteRules() (map[string]ProviderRouteRule, error) {
	registered, err := RegisteredTransportOperations()
	if err != nil {
		return nil, err
	}
	return buildProviderRouteRules(registered, providerRouteDeclarations)
}

// ProviderRouteRules returns a detached ownership table. Registration and
// tests guarantee that construction succeeds for the checked-in route graph.
func ProviderRouteRules() map[string]ProviderRouteRule {
	rules, err := providerRouteRules()
	if err != nil {
		panic(err)
	}
	return maps.Clone(rules)
}

func buildProviderRouteRules(
	registered []RegisteredTransportOperation,
	declarations []ProviderRouteRule,
) (map[string]ProviderRouteRule, error) {
	operations := make(map[string]RegisteredTransportOperation, len(registered))
	for _, operation := range registered {
		if _, duplicate := operations[operation.ID]; duplicate {
			return nil, fmt.Errorf("duplicate registered operation %q", operation.ID)
		}
		operations[operation.ID] = operation
	}

	rules := make(map[string]ProviderRouteRule, len(declarations))
	for _, declaration := range declarations {
		operation, known := operations[declaration.OperationID]
		if !known {
			return nil, fmt.Errorf("ownership declares unknown operation %q", declaration.OperationID)
		}
		if _, duplicate := rules[declaration.OperationID]; duplicate {
			return nil, fmt.Errorf("duplicate ownership for operation %q", declaration.OperationID)
		}
		switch declaration.Owner {
		case ProviderHubOnly, ProviderWithLocalOverlay:
			if declaration.PeerScope != federationauth.ScopeProviderRead &&
				declaration.PeerScope != federationauth.ScopeProviderWrite &&
				declaration.PeerScope != federationauth.ScopeProviderHandoff {
				return nil, fmt.Errorf(
					"provider operation %q requires provider scope",
					declaration.OperationID,
				)
			}
		case NodeLocal:
			if declaration.PeerScope != "" {
				return nil, fmt.Errorf(
					"spoke-local operation %q must use the separate peer inventory",
					declaration.OperationID,
				)
			}
			if operation.PeerCallable {
				declaration.PeerScope = operation.PeerScope
			}
		default:
			return nil, fmt.Errorf(
				"operation %q has invalid owner %d",
				declaration.OperationID, declaration.Owner,
			)
		}
		rules[declaration.OperationID] = declaration
	}
	for _, operation := range registered {
		if _, ok := rules[operation.ID]; !ok {
			return nil, fmt.Errorf("operation %s has no ownership", operation.ID)
		}
	}
	return rules, nil
}

type providerRouteMatch struct {
	operation RegisteredTransportOperation
	rule      ProviderRouteRule
}

var providerRouteIndex = sync.OnceValues(func() ([]providerRouteMatch, error) {
	operations, err := RegisteredTransportOperations()
	if err != nil {
		return nil, err
	}
	rules, err := buildProviderRouteRules(operations, providerRouteDeclarations)
	if err != nil {
		return nil, err
	}
	matches := make([]providerRouteMatch, 0, len(operations))
	for _, operation := range operations {
		matches = append(matches, providerRouteMatch{
			operation: operation,
			rule:      rules[operation.ID],
		})
	}
	return matches, nil
})

func providerRouteRuleForRequest(
	method string, canonicalPath string,
) (ProviderRouteRule, bool) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	matches, err := providerRouteIndex()
	if err != nil {
		panic(fmt.Sprintf("build provider route index: %v", err))
	}
	for _, match := range matches {
		if match.operation.Method == method &&
			providerPathMatches(match.operation.Path, canonicalPath) {
			return match.rule, true
		}
	}
	return ProviderRouteRule{}, false
}

func providerPathMatches(pattern, path string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) != len(pathParts) {
		return false
	}
	for index, part := range patternParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			if pathParts[index] == "" {
				return false
			}
			continue
		}
		if part != pathParts[index] {
			return false
		}
	}
	return true
}
