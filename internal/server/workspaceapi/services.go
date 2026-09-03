package workspaceapi

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/workspace/localruntime"
)

type CreatePullWorkspaceRequest struct {
	Provider           string
	PlatformHost       string
	Owner              string
	Name               string
	Number             int
	SuppressAutoAssign bool
}

type CreateIssueWorkspaceRequest struct {
	Provider               string
	PlatformHost           string
	Owner                  string
	Name                   string
	Number                 int
	GitHeadRef             *string
	ReuseExistingBranch    bool
	ReuseExistingDirectory bool
	SuppressAutoAssign     bool
}

type CreateAdHocWorkspaceRequest struct {
	Provider            string
	PlatformHost        string
	Owner               string
	Name                string
	Branch              *string
	ReuseExistingBranch bool
}

type ProviderWorkspaceItemRequest struct {
	Repository providerplane.RepositoryRoute `json:"repository"`
	ItemType   string                        `json:"item_type"`
	ItemNumber int                           `json:"item_number"`
}

type ProviderWorkspaceAutomation interface {
	AutoAssignWorkspaceItem(context.Context, ProviderWorkspaceItemRequest) error
}

type MergeRequestWorktreeFacts struct {
	Number           int
	URL              string
	State            string
	Title            string
	IsDraft          bool
	HeadBranch       string
	HeadRepoCloneURL string
	ExpectedHeadSHA  string
}

type MergeRequestWorktreeSource interface {
	ResolveMergeRequestWorktreeFacts(
		context.Context, providerplane.RepositoryRoute, int,
	) (MergeRequestWorktreeFacts, error)
}

type WorkspaceResult struct {
	Workspace WorkspaceResponse
}

type WorkspaceRuntimeResult struct {
	LaunchTargets []localruntime.LaunchTarget
	Sessions      []localruntime.SessionInfo
}

type AgentSessionResult struct {
	Agent             string
	SessionID         string
	RuntimeSessionKey string
	TargetKey         string
	State             agentactivity.State
	UpdatedAt         time.Time
	InitialMessage    *InitialMessageResult
}

type InitialMessageRequest struct {
	WorkspaceID       string
	RuntimeSessionKey string
	TargetKey         string
	Message           string
}

type InitialMessageResult struct {
	TargetKey    string
	State        string
	MessageBytes int
	ReservedAt   time.Time
	DeliveredAt  *time.Time
}

type AgentMessageResult struct {
	TargetKey    string
	MessageBytes int
	SubmittedAt  time.Time
}

func (s *Handler) resolveWorkspaceLaunchSpec(
	ctx context.Context,
	route providerplane.RepositoryRoute,
	itemType string,
	itemNumber int,
	gitHeadRef string,
	issueBranchSlug bool,
) (db.WorkspaceLaunchSpec, error) {
	if s.launchSpecResolver == nil {
		return db.WorkspaceLaunchSpec{}, providerplane.ErrHubUnavailable
	}
	return s.launchSpecResolver.ResolveWorkspaceLaunchSpec(
		ctx,
		providerplane.WorkspaceLaunchRequest{
			Repository: route, ItemType: itemType, ItemNumber: itemNumber,
			GitHeadRef: gitHeadRef, IssueBranchSlug: issueBranchSlug,
		},
	)
}

// RefreshProviderWorkspaceFacts updates hub-owned provider state for
// a spoke's manual workspace refresh. It never writes spoke-local workspace
// state; the spoke persists the returned launch specification separately.
func (s *Handler) RefreshProviderWorkspaceFacts(
	ctx context.Context, request providerplane.WorkspaceLaunchRequest,
) error {
	if s.syncer == nil {
		return httpapi.ServiceUnavailable("syncer not configured")
	}
	route, err := providerplane.CanonicalRepositoryRoute(request.Repository)
	if err != nil {
		return httpapi.BadRequest(httpapi.CodeValidationError, err.Error(), nil)
	}
	var repo *db.Repo
	if platformRepoID := strings.TrimSpace(request.PlatformRepoID); platformRepoID != "" {
		entry, lookupErr := s.db.GetRepositoryByProviderID(
			ctx, route.Provider, route.PlatformHost, platformRepoID,
		)
		if lookupErr != nil {
			return providerRouteLookupError(lookupErr)
		}
		if entry != nil && entry.Lifecycle == db.RepositoryLifecycleActive {
			repo = &entry.Repository
		}
	} else {
		repo, err = s.lookupRepoByProviderRoute(
			ctx, route.Provider, route.PlatformHost, route.Owner, route.Name,
		)
		if err != nil {
			return providerRouteLookupError(err)
		}
	}
	if repo == nil {
		return httpapi.NotFound(httpapi.CodeRepoNotFound, "repo not found", nil)
	}
	kind := repoProviderKind(*repo)
	host := repoProviderHost(*repo)
	if request.ItemType == db.WorkspaceItemTypeIssue {
		return s.refreshWorkspaceIssue(
			ctx, repo.ID, kind, host, repo.Owner, repo.Name, request.ItemNumber,
		)
	}
	if request.ItemType == db.WorkspaceItemTypePullRequest {
		return s.refreshWorkspacePullRequest(
			ctx, repo.ID, kind, host, repo.Owner, repo.Name, request.ItemNumber,
		)
	}
	return nil
}
