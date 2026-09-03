package workspaceapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/forge/internal/agentactivity"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/platform"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/internal/workspace/localruntime"
	gitcmd "go.kenn.io/kit/git/cmd"
)

type createWorkspaceInput struct {
	Body struct {
		Provider           string `json:"provider"`
		PlatformHost       string `json:"platform_host"`
		Owner              string `json:"owner"`
		Name               string `json:"name"`
		MRNumber           int    `json:"mr_number"`
		SuppressAutoAssign bool   `json:"suppress_auto_assign,omitempty"`
	}
}

type createIssueWorkspaceInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Number       int    `path:"number"`
	Body         struct {
		GitHeadRef             *string `json:"git_head_ref,omitempty"`
		ReuseExistingBranch    bool    `json:"reuse_existing_branch,omitempty"`
		ReuseExistingDirectory bool    `json:"reuse_existing_directory,omitempty"`
		SuppressAutoAssign     bool    `json:"suppress_auto_assign,omitempty"`
	}
}

// CreateIssueWorkspaceInput is shared with provider-aware route wrappers.
type CreateIssueWorkspaceInput = createIssueWorkspaceInput

type createAdHocWorkspaceInput struct {
	Provider     string `path:"provider"`
	PlatformHost string
	Owner        string `path:"owner"`
	Name         string `path:"name"`
	Body         struct {
		Branch              *string `json:"branch,omitempty" doc:"Branch for the new worktree; generated when empty"`
		ReuseExistingBranch bool    `json:"reuse_existing_branch,omitempty"`
	}
}

// CreateAdHocWorkspaceInput is shared with provider-aware route wrappers.
type CreateAdHocWorkspaceInput = createAdHocWorkspaceInput

const issueWorkspaceBranchConflictType = "urn:kenn-forge:error:issue-workspace-branch-conflict"

const adHocWorkspaceBranchConflictType = "urn:kenn-forge:error:workspace-branch-conflict"

type getWorkspaceInput struct {
	ID string `path:"id"`
}

type getWorkspaceFilesInput struct {
	ID         string `path:"id"`
	Base       string `query:"base"      doc:"Diff base: head, pushed, or merge-target"`
	Whitespace string `query:"whitespace" doc:"Set to hide to ignore whitespace-only changes"`
	Commit     string `query:"commit" doc:"Scope to a single commit SHA"`
	From       string `query:"from"   doc:"Start SHA for range diff (inclusive)"`
	To         string `query:"to"     doc:"End SHA for range diff (inclusive)"`
}

type watchWorkspaceDiffInput struct {
	ID      string `path:"id"`
	Version string `query:"version" doc:"Last observed opaque workspace diff snapshot version"`
}

type getWorkspaceDiffInput struct {
	ID         string `path:"id"`
	Base       string `query:"base"      doc:"Diff base: head, pushed, or merge-target"`
	Whitespace string `query:"whitespace" doc:"Set to hide to ignore whitespace-only changes"`
	Path       string `query:"path"      doc:"Optional file path to limit the returned patch"`
	Commit     string `query:"commit" doc:"Scope to a single commit SHA"`
	From       string `query:"from"   doc:"Start SHA for range diff (inclusive)"`
	To         string `query:"to"     doc:"End SHA for range diff (inclusive)"`
	Revision   string `query:"revision" doc:"Optional snapshot_version returned by the workspace files endpoint"`
}

type getWorkspaceFilePreviewInput struct {
	ID         string `path:"id"`
	Base       string `query:"base"       enum:"head,pushed,merge-target" doc:"Diff base: head, pushed, or merge-target"`
	Whitespace string `query:"whitespace" enum:"hide"                     doc:"Set to hide to ignore whitespace-only changes"`
	Path       string `query:"path"       doc:"Changed file path to preview"`
	Side       string `query:"side"       enum:"old,new"                  doc:"Optional diff side to read for context expansion"`
	Commit     string `query:"commit" doc:"Scope to a single commit SHA"`
	From       string `query:"from"   doc:"Start SHA for range diff (inclusive)"`
	To         string `query:"to"     doc:"End SHA for range diff (inclusive)"`
	Revision   string `query:"revision" doc:"Optional snapshot_version returned by the workspace files endpoint"`
}

type getWorkspaceCommitsInput struct {
	ID string `path:"id"`
}

type retryWorkspaceInput struct {
	ID string `path:"id"`
}

type refreshWorkspaceInput struct {
	ID string `path:"id"`
}

type getWorkspaceRuntimeInput struct {
	ID string `path:"id"`
}

type launchWorkspaceRuntimeSessionInput struct {
	ID   string `path:"id"`
	Body struct {
		TargetKey     string `json:"target_key"`
		DisplayRegion string `json:"display_region,omitempty"`
	}
}

// LaunchWorkspaceRuntimeSessionInput is the programmatic launch contract.
type LaunchWorkspaceRuntimeSessionInput = launchWorkspaceRuntimeSessionInput

type stopWorkspaceRuntimeSessionInput struct {
	ID         string `path:"id"`
	SessionKey string `path:"session_key"`
}

type renameWorkspaceRuntimeSessionInput struct {
	ID         string `path:"id"`
	SessionKey string `path:"session_key"`
	Body       struct {
		Label string `json:"label"`
	}
}

type getWorkspaceRuntimeSessionAttachSpecInput struct {
	ID         string `path:"id"`
	SessionKey string `path:"session_key"`
}

type DeleteWorkspaceInput struct {
	ID    string `path:"id"`
	Force bool   `query:"force"`
}

type listWorkspacesOutputBody struct {
	Workspaces []workspaceResponse `json:"workspaces"`
}

type listWorkspacesOutput = httpapi.BodyOutput[listWorkspacesOutputBody]

type getWorkspaceOutput = httpapi.BodyOutput[workspaceResponse]

type getWorkspaceDiffOutput = httpapi.BodyOutput[diffResponse]
type getWorkspaceFilePreviewOutput = httpapi.BodyOutput[filePreviewResponse]
type getWorkspaceFilesOutput = httpapi.BodyOutput[filesResponse]
type watchWorkspaceDiffOutput = httpapi.BodyOutput[workspaceDiffWatchResponse]
type getWorkspaceCommitsOutput = httpapi.BodyOutput[commitsResponse]

type getWorkspaceRuntimeOutput = httpapi.BodyOutput[workspaceRuntimeResponse]

type workspaceRuntimeSessionOutput = httpapi.BodyOutput[localruntime.SessionInfo]

type runtimeAttachSpecOutput = httpapi.BodyOutput[runtimeAttachSpecResponse]

type createWorkspaceOutput = httpapi.AcceptedBodyOutput[workspaceResponse]

// CreateWorkspaceOutput is shared with Kata and provider-aware route wrappers.
type CreateWorkspaceOutput = createWorkspaceOutput

type refreshWorkspaceOutput = httpapi.BodyOutput[workspaceResponse]

type workspaceDiffRequest struct {
	Summary           *db.WorkspaceSummary
	Base              workspace.WorktreeDiffBase
	MergeTargetBranch string
	FromSHA           string
	ToSHA             string
}

// --- Workspaces ---

// createWorkspace creates or reuses a PR-backed kenn-forge workspace.
//
// This API exists so a tracked pull request can have a durable local execution
// context that kenn-forge owns and can reopen later. It is not a generic
// worktree-creation endpoint for arbitrary branches.
func (s *Handler) createWorkspace(
	ctx context.Context, input *createWorkspaceInput,
) (*createWorkspaceOutput, error) {
	result, err := s.CreatePullWorkspace(ctx, CreatePullWorkspaceRequest{
		Provider: input.Body.Provider, PlatformHost: input.Body.PlatformHost,
		Owner: input.Body.Owner, Name: input.Body.Name, Number: input.Body.MRNumber,
		SuppressAutoAssign: input.Body.SuppressAutoAssign,
	})
	if err != nil {
		return nil, err
	}
	return &createWorkspaceOutput{Status: http.StatusAccepted, Body: result.Workspace}, nil
}

func (s *Handler) CreatePullWorkspace(
	ctx context.Context, req CreatePullWorkspaceRequest,
) (WorkspaceResult, error) {
	input := &createWorkspaceInput{}
	input.Body.Provider = req.Provider
	input.Body.PlatformHost = req.PlatformHost
	input.Body.Owner = req.Owner
	input.Body.Name = req.Name
	input.Body.MRNumber = req.Number
	input.Body.SuppressAutoAssign = req.SuppressAutoAssign
	output, err := s.createPullWorkspaceRouteCore(ctx, input)
	if err != nil {
		return WorkspaceResult{}, err
	}
	return WorkspaceResult{Workspace: output.Body}, nil
}

func (s *Handler) createPullWorkspaceRouteCore(
	ctx context.Context, input *createWorkspaceInput,
) (*createWorkspaceOutput, error) {
	if s.workspaces == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}
	provider, err := s.createWorkspaceProvider(ctx, input)
	if err != nil {
		return nil, err
	}
	spec, err := s.resolveWorkspaceLaunchSpec(
		ctx,
		providerplane.RepositoryRoute{
			Provider: provider, PlatformHost: input.Body.PlatformHost,
			Owner: input.Body.Owner, Name: input.Body.Name,
		},
		db.WorkspaceItemTypePullRequest, input.Body.MRNumber, "", false,
	)
	if err != nil {
		return nil, workspaceLaunchSpecProblem(err)
	}
	ws, err := s.workspaces.CreateFromLaunchSpec(ctx, spec)
	if err != nil {
		if errors.Is(err, workspace.ErrLaunchSpecSourceHidden) ||
			errors.Is(err, workspace.ErrLaunchSpecRefreshRequired) {
			return nil, workspaceLaunchSpecProblem(err)
		}
		if problem := repositoryRouteFenceProblem(err); problem != nil {
			return nil, problem
		}
		if errors.Is(err, workspace.ErrWorkspaceNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceNotSynced) {
			return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceDuplicate) {
			return nil, httpapi.Conflict(httpapi.CodeWorkspaceAlreadyExists,
				"workspace already exists for this MR", nil)
		}
		return nil, httpapi.Internal("create workspace: " + err.Error())
	}

	if assignErr := s.autoAssignWorkspaceItemForRoute(ctx, ProviderWorkspaceItemRequest{
		Repository: providerplane.RepositoryRoute{
			Provider: spec.Repository.Provider, PlatformHost: spec.Repository.PlatformHost,
			Owner: spec.Repository.Owner, Name: spec.Repository.Name,
		},
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: input.Body.MRNumber,
	}, input.Body.SuppressAutoAssign); assignErr != nil {
		slog.Warn("automatically assign pull request workspace",
			"provider", spec.Repository.Provider,
			"platform_host", spec.Repository.PlatformHost,
			"owner", spec.Repository.Owner,
			"name", spec.Repository.Name,
			"number", input.Body.MRNumber,
			"err", assignErr,
		)
	}

	s.runWorkspaceSetup(ws)

	summary, err := s.workspaces.GetSummary(ctx, ws.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace summary: " + err.Error())
	}
	if summary == nil {
		return nil, httpapi.Internal("workspace summary missing after create")
	}
	resp := s.toWorkspaceResponse(ctx, summary)
	resp.Created = true
	return &createWorkspaceOutput{
		Status: http.StatusAccepted,
		Body:   resp,
	}, nil
}

func workspaceLaunchSpecProblem(err error) error {
	if problem, ok := errors.AsType[*httpapi.ProblemError](err); ok {
		return problem
	}
	switch {
	case errors.Is(err, workspace.ErrLaunchSpecSourceHidden):
		return httpapi.NotFound(
			httpapi.CodeNotFound, "workspace source item is not visible", nil,
		)
	case errors.Is(err, providerplane.ErrHubUnavailable),
		errors.Is(err, providerplane.ErrCredentialUnavailable),
		errors.Is(err, workspace.ErrLaunchSpecResolverMissing),
		errors.Is(err, workspace.ErrLaunchSpecRefreshRequired):
		return httpapi.HubUnavailable(
			"workspace provider facts are unavailable because the federation hub cannot be reached",
		)
	default:
		return httpapi.Internal("resolve workspace launch specification: " + err.Error())
	}
}

func (s *Handler) createWorkspaceProvider(
	ctx context.Context, input *createWorkspaceInput,
) (string, error) {
	if strings.TrimSpace(input.Body.Provider) != "" {
		provider, err := normalizeRouteProvider(input.Body.Provider)
		if err != nil {
			return "", httpapi.Validation("body.provider", err.Error())
		}
		return provider, nil
	}
	_ = ctx
	return "", httpapi.Validation("body.provider", "provider is required")
}

func (s *Handler) runWorkspaceSetup(ws *workspace.Workspace) {
	s.runWorkspaceSetupWithBasePath(ws, "")
}

func (s *Handler) runWorkspaceSetupWithBasePath(ws *workspace.Workspace, basePath string) {
	done, start := s.beginWorkspaceSetup(ws.ID)
	if done == nil {
		return
	}
	if !start {
		s.runBackground(func(ctx context.Context) {
			select {
			case <-done:
				s.runWorkspaceSetupWithBasePath(ws, basePath)
			case <-ctx.Done():
			}
		})
		return
	}
	if !s.runBackground(func(bgCtx context.Context) {
		defer s.finishWorkspaceSetup(ws.ID, done)
		for {
			setupErr := s.workspaces.SetupWithOptions(bgCtx, ws, workspace.SetupOptions{
				WorktreeBasePath:         basePath,
				RoborevInitManagedClones: s.configSnapshot().RoborevInitManagedClones,
			})
			summary, getErr := s.workspaces.GetSummary(
				bgCtx, ws.ID,
			)
			if getErr != nil {
				slog.Warn("get workspace summary after setup",
					"id", ws.ID, "err", getErr,
				)
				return
			}
			if summary == nil {
				return
			}
			resp := s.toWorkspaceResponse(bgCtx, summary)
			if setupErr != nil {
				slog.Warn("workspace setup failed",
					"id", ws.ID, "err", setupErr,
				)
			}
			s.hub.Broadcast(Event{
				Type: "workspace_status",
				Data: resp,
			})
			if setupErr == nil {
				s.runWorkspacePushedHeadObserverPass(bgCtx)
			}
			if errors.Is(setupErr, workspace.ErrWorkspaceRepositoryUnresolved) {
				if deleteErr := s.queueWorkspaceRetirement(ws.ID); deleteErr != nil {
					slog.Warn("delete unresolved workspace", "id", ws.ID, "err", deleteErr)
				}
				return
			}

			next, queued, queueErr := s.workspaces.StartQueuedRetryIfErrored(
				bgCtx, ws.ID,
			)
			if queueErr != nil {
				slog.Warn("start queued workspace retry",
					"id", ws.ID, "err", queueErr,
				)
				summary, getErr = s.workspaces.GetSummary(bgCtx, ws.ID)
				if getErr != nil {
					slog.Warn("get workspace summary after queued retry failure",
						"id", ws.ID, "err", getErr,
					)
					return
				}
				if summary != nil {
					s.hub.Broadcast(Event{
						Type: "workspace_status",
						Data: s.toWorkspaceResponse(bgCtx, summary),
					})
				}
				return
			}
			if !queued {
				return
			}
			if next == nil {
				return
			}
			ws = next
			summary, getErr = s.workspaces.GetSummary(bgCtx, ws.ID)
			if getErr != nil {
				slog.Warn("get workspace summary after queued retry",
					"id", ws.ID, "err", getErr,
				)
				return
			}
			if summary == nil {
				return
			}
			s.hub.Broadcast(Event{
				Type: "workspace_status",
				Data: s.toWorkspaceResponse(bgCtx, summary),
			})
		}
	}) {
		s.finishWorkspaceSetup(ws.ID, done)
	}
}

// workspaceDeletion tracks concurrent DELETE requests for one workspace ID.
// Setup admission stays closed while any deletion is active and permanently
// once one succeeds; done is closed when the last active deletion finishes so
// setup dispatchers queued behind a failed deletion can replay.
type workspaceDeletion struct {
	active    int
	succeeded bool
	closed    bool
	done      chan struct{}
}

func (s *Handler) beginWorkspaceSetup(workspaceID string) (chan struct{}, bool) {
	s.workspaceSetupMu.Lock()
	defer s.workspaceSetupMu.Unlock()
	if deletion := s.workspaceDeleting[workspaceID]; deletion != nil {
		if deletion.succeeded {
			return nil, false
		}
		return deletion.done, false
	}
	if done, ok := s.workspaceSetupDone[workspaceID]; ok {
		return done, false
	}
	done := make(chan struct{})
	s.workspaceSetupDone[workspaceID] = done
	return done, true
}

func (s *Handler) finishWorkspaceSetup(workspaceID string, done chan struct{}) {
	s.workspaceSetupMu.Lock()
	if s.workspaceSetupDone[workspaceID] == done {
		delete(s.workspaceSetupDone, workspaceID)
		close(done)
	}
	s.workspaceSetupMu.Unlock()
}

func (s *Handler) markWorkspaceDeleting(workspaceID string) <-chan struct{} {
	s.workspaceSetupMu.Lock()
	defer s.workspaceSetupMu.Unlock()
	deletion := s.workspaceDeleting[workspaceID]
	if deletion == nil {
		deletion = &workspaceDeletion{done: make(chan struct{})}
		s.workspaceDeleting[workspaceID] = deletion
	}
	deletion.active++
	return s.workspaceSetupDone[workspaceID]
}

func (s *Handler) finishWorkspaceDeleting(workspaceID string, succeeded bool) {
	s.workspaceSetupMu.Lock()
	defer s.workspaceSetupMu.Unlock()
	deletion := s.workspaceDeleting[workspaceID]
	if deletion == nil {
		return
	}
	deletion.active--
	if succeeded {
		deletion.succeeded = true
	}
	if deletion.active > 0 {
		return
	}
	if !deletion.closed {
		close(deletion.done)
		deletion.closed = true
	}
	if !deletion.succeeded {
		delete(s.workspaceDeleting, workspaceID)
	}
}

func waitForWorkspaceSetup(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunWorkspaceSetupWithBasePath starts asynchronous materialization for a
// workspace created by a dependent domain such as Kata.
func (s *Handler) RunWorkspaceSetupWithBasePath(ws *workspace.Workspace, basePath string) {
	s.runWorkspaceSetupWithBasePath(ws, basePath)
}

// PublishWorkspaceCreated announces the durable identity before asynchronous
// setup can publish its first status transition.
func (s *Handler) PublishWorkspaceCreated(workspaceID string, created bool) {
	s.hub.Broadcast(Event{
		Type: "workspace_created",
		Data: map[string]any{"id": workspaceID, "created": created},
	})
}

// createIssueWorkspace creates or reuses an issue-backed kenn-forge workspace.
//
// This API exists so an issue can have its own durable local execution context
// even when there is no PR branch yet. These workspaces start from the repo's
// current origin/HEAD and are presented in the UI with issue-specific sidebar
// behavior instead of PR/reviews affordances.
func (s *Handler) createIssueWorkspace(
	ctx context.Context, input *createIssueWorkspaceInput,
) (*createWorkspaceOutput, error) {
	result, err := s.CreateIssueWorkspaceService(ctx, CreateIssueWorkspaceRequest{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Number: input.Number,
		GitHeadRef:             input.Body.GitHeadRef,
		ReuseExistingBranch:    input.Body.ReuseExistingBranch,
		ReuseExistingDirectory: input.Body.ReuseExistingDirectory,
		SuppressAutoAssign:     input.Body.SuppressAutoAssign,
	})
	if err != nil {
		return nil, err
	}
	return &createWorkspaceOutput{Status: http.StatusAccepted, Body: result.Workspace}, nil
}

func (s *Handler) CreateIssueWorkspaceService(
	ctx context.Context, req CreateIssueWorkspaceRequest,
) (WorkspaceResult, error) {
	input := &createIssueWorkspaceInput{
		Provider: req.Provider, PlatformHost: req.PlatformHost,
		Owner: req.Owner, Name: req.Name, Number: req.Number,
	}
	input.Body.GitHeadRef = req.GitHeadRef
	input.Body.ReuseExistingBranch = req.ReuseExistingBranch
	input.Body.ReuseExistingDirectory = req.ReuseExistingDirectory
	input.Body.SuppressAutoAssign = req.SuppressAutoAssign
	output, err := s.createIssueWorkspaceRouteCore(ctx, input)
	if err != nil {
		return WorkspaceResult{}, err
	}
	return WorkspaceResult{Workspace: output.Body}, nil
}

func (s *Handler) createIssueWorkspaceRouteCore(
	ctx context.Context, input *createIssueWorkspaceInput,
) (*createWorkspaceOutput, error) {
	if s.workspaces == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}
	if input.Body.ReuseExistingBranch && input.Body.ReuseExistingDirectory {
		return nil, httpapi.Validation(
			"body.reuse_existing_directory",
			"reuse_existing_branch and reuse_existing_directory are mutually exclusive",
		)
	}
	provider, err := normalizeRouteProvider(input.Provider)
	if err != nil {
		return nil, httpapi.Validation("path.provider", err.Error())
	}

	existing, err := s.workspaces.GetByIssueForProvider(
		ctx,
		provider, input.PlatformHost, input.Owner, input.Name,
		input.Number,
	)
	if err != nil {
		return nil, httpapi.Internal("lookup existing issue workspace: " + err.Error())
	}
	if existing != nil {
		summary, getErr := s.workspaces.GetSummary(ctx, existing.ID)
		if getErr != nil {
			return nil, httpapi.Internal("get workspace summary: " + getErr.Error())
		}
		if summary == nil {
			return nil, httpapi.Internal("workspace summary missing for existing workspace")
		}
		return &createWorkspaceOutput{
			Status: http.StatusAccepted,
			Body:   s.toWorkspaceResponse(ctx, summary),
		}, nil
	}
	spec, err := s.resolveWorkspaceLaunchSpec(
		ctx,
		providerplane.RepositoryRoute{
			Provider: provider, PlatformHost: input.PlatformHost,
			Owner: input.Owner, Name: input.Name,
		},
		db.WorkspaceItemTypeIssue, input.Number,
		strings.TrimSpace(derefString(input.Body.GitHeadRef)),
		s.configSnapshot().IssueBranchSlug,
	)
	if err != nil {
		return nil, workspaceLaunchSpecProblem(err)
	}
	existing, err = s.workspaces.GetByLaunchSpecIdentity(ctx, spec)
	if err != nil {
		return nil, httpapi.Internal("lookup existing issue workspace identity: " + err.Error())
	}
	if existing != nil {
		summary, getErr := s.workspaces.GetSummary(ctx, existing.ID)
		if getErr != nil {
			return nil, httpapi.Internal("get workspace summary: " + getErr.Error())
		}
		if summary == nil {
			return nil, httpapi.Internal("workspace summary missing for existing workspace")
		}
		return &createWorkspaceOutput{
			Status: http.StatusAccepted,
			Body:   s.toWorkspaceResponse(ctx, summary),
		}, nil
	}
	ws, err := s.workspaces.CreateIssueFromLaunchSpec(
		ctx, spec,
		workspace.CreateIssueOptions{
			Provider:               provider,
			GitHeadRef:             strings.TrimSpace(derefString(input.Body.GitHeadRef)),
			ReuseExistingBranch:    input.Body.ReuseExistingBranch,
			ReuseExistingDirectory: input.Body.ReuseExistingDirectory,
		},
	)
	if err != nil {
		if errors.Is(err, workspace.ErrLaunchSpecSourceHidden) ||
			errors.Is(err, workspace.ErrLaunchSpecRefreshRequired) {
			return nil, workspaceLaunchSpecProblem(err)
		}
		if problem := repositoryRouteFenceProblem(err); problem != nil {
			return nil, problem
		}
		msg := err.Error()
		if recoveryErr, ok := errors.AsType[*workspace.WorkspaceDirectoryRecoveryError](err); ok {
			details := map[string]any{
				"reason": string(recoveryErr.Reason),
			}
			if recoveryErr.Reason == workspace.WorkspaceDirectoryBranchMismatch {
				details["expectedBranch"] = recoveryErr.ExpectedBranch
				details["actualBranch"] = recoveryErr.ActualBranch
			}
			return nil, httpapi.Conflict(
				httpapi.CodeWorkspaceDirectoryNotReusable,
				recoveryErr.Error(),
				details,
			)
		}
		if branchConflict, ok := errors.AsType[*workspace.WorkspaceBranchConflictError](err); ok {
			// Branch-conflict gets the typed problem envelope with
			// Type carrying the URN and Details carrying the conflicting
			// branch + suggested alternative. The legacy Errors[]
			// entries are populated for callers still introspecting
			// per-field huma error details.
			conflict := httpapi.NewProblem(
				http.StatusConflict,
				httpapi.CodeBranchConflict,
				"A local branch with the requested name already exists.",
				map[string]any{
					"branch":            branchConflict.Branch,
					"suggestedBranch":   branchConflict.SuggestedBranch,
					"existingDirectory": branchConflict.ExistingDirectory,
				},
			)
			conflict.Type = issueWorkspaceBranchConflictType
			conflict.Title = "Issue workspace branch conflict"
			conflict.Errors = []*huma.ErrorDetail{
				{
					Message:  "Requested branch already exists",
					Location: "body.git_head_ref",
					Value:    branchConflict.Branch,
				},
				{
					Message:  "Suggested alternative branch name",
					Location: "body.suggested_git_head_ref",
					Value:    branchConflict.SuggestedBranch,
				},
			}
			return nil, conflict
		}
		if strings.Contains(msg, "not tracked") {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, msg, nil)
		}
		if strings.Contains(msg, "not synced") {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, msg, nil)
		}
		if strings.Contains(msg, "invalid branch name") {
			return nil, httpapi.Validation("body.git_head_ref", msg)
		}
		if strings.Contains(msg, "UNIQUE constraint") {
			existing, getErr := s.workspaces.GetByIssueForProvider(
				ctx,
				spec.Repository.Provider,
				spec.Repository.PlatformHost,
				spec.Repository.Owner,
				spec.Repository.Name,
				input.Number,
			)
			if getErr == nil && existing != nil {
				summary, summaryErr := s.workspaces.GetSummary(ctx, existing.ID)
				if summaryErr == nil && summary != nil {
					return &createWorkspaceOutput{
						Status: http.StatusAccepted,
						Body:   s.toWorkspaceResponse(ctx, summary),
					}, nil
				}
			}
		}
		return nil, httpapi.Internal("create issue workspace: " + msg)
	}

	createdBranch := ws.WorkspaceBranch != ""
	if assignErr := s.autoAssignWorkspaceItemForRoute(ctx, ProviderWorkspaceItemRequest{
		Repository: providerplane.RepositoryRoute{
			Provider: spec.Repository.Provider, PlatformHost: spec.Repository.PlatformHost,
			Owner: spec.Repository.Owner, Name: spec.Repository.Name,
		},
		ItemType: db.WorkspaceItemTypeIssue, ItemNumber: input.Number,
	}, input.Body.SuppressAutoAssign); assignErr != nil {
		slog.Warn("automatically assign issue workspace",
			"provider", spec.Repository.Provider,
			"platform_host", spec.Repository.PlatformHost,
			"owner", spec.Repository.Owner,
			"name", spec.Repository.Name,
			"number", input.Number,
			"err", assignErr,
		)
	}
	s.runWorkspaceSetup(ws)

	summary, err := s.workspaces.GetSummary(ctx, ws.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace summary: " + err.Error())
	}
	if summary == nil {
		return nil, httpapi.Internal("workspace summary missing after create")
	}

	resp := s.toWorkspaceResponse(ctx, summary)
	resp.Created = createdBranch
	return &createWorkspaceOutput{
		Status: http.StatusAccepted,
		Body:   resp,
	}, nil
}

// CreateIssueWorkspace creates or reuses an issue-backed workspace.
func (s *Handler) CreateIssueWorkspace(
	ctx context.Context, input *CreateIssueWorkspaceInput,
) (*CreateWorkspaceOutput, error) {
	return s.createIssueWorkspace(ctx, input)
}

// createAdHocWorkspace creates or reuses a workspace for new work in a tracked
// repository.
//
// This API exists so a maintainer can start work that has no pull request,
// issue, or Kata task behind it without inventing one first. The branch is the
// workspace's identity, so requesting the same branch twice returns the first
// workspace instead of a second worktree. Like issue-backed workspaces these
// start from the repository's current origin/HEAD.
func (s *Handler) createAdHocWorkspace(
	ctx context.Context, input *createAdHocWorkspaceInput,
) (*createWorkspaceOutput, error) {
	result, err := s.CreateAdHocWorkspaceService(ctx, CreateAdHocWorkspaceRequest{
		Provider: input.Provider, PlatformHost: input.PlatformHost,
		Owner: input.Owner, Name: input.Name, Branch: input.Body.Branch,
		ReuseExistingBranch: input.Body.ReuseExistingBranch,
	})
	if err != nil {
		return nil, err
	}
	return &createWorkspaceOutput{Status: http.StatusAccepted, Body: result.Workspace}, nil
}

func (s *Handler) CreateAdHocWorkspaceService(
	ctx context.Context, req CreateAdHocWorkspaceRequest,
) (WorkspaceResult, error) {
	if s.resolveRepository != nil {
		if _, err := s.resolveRepository(ctx, providerplane.RepositoryRoute{
			Provider: req.Provider, PlatformHost: req.PlatformHost,
			Owner: req.Owner, Name: req.Name,
		}); err != nil {
			return WorkspaceResult{}, err
		}
	}
	input := &createAdHocWorkspaceInput{
		Provider: req.Provider, PlatformHost: req.PlatformHost,
		Owner: req.Owner, Name: req.Name,
	}
	input.Body.Branch = req.Branch
	input.Body.ReuseExistingBranch = req.ReuseExistingBranch
	output, err := s.createAdHocWorkspaceRouteCore(ctx, input)
	if err != nil {
		return WorkspaceResult{}, err
	}
	return WorkspaceResult{Workspace: output.Body}, nil
}

func (s *Handler) createAdHocWorkspaceRouteCore(
	ctx context.Context, input *createAdHocWorkspaceInput,
) (*createWorkspaceOutput, error) {
	if s.workspaces == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}
	repo, err := s.lookupRepoByProviderRoute(
		ctx, input.Provider, input.PlatformHost, input.Owner, input.Name,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}

	branch := strings.TrimSpace(derefString(input.Body.Branch))
	itemKey := db.AdHocWorkspaceItemKey(branch)
	if itemKey != "" {
		existing, err := s.adHocWorkspaceForBranch(ctx, repo, itemKey)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}

	ws, err := s.workspaces.CreateAdHoc(
		ctx,
		repo.Platform,
		repo.PlatformHost,
		repo.Owner,
		repo.Name,
		workspace.CreateAdHocOptions{
			BranchName:          branch,
			ReuseExistingBranch: input.Body.ReuseExistingBranch,
		},
	)
	if err != nil {
		return s.adHocWorkspaceCreateError(ctx, repo, itemKey, err)
	}

	createdBranch := ws.WorkspaceBranch != ""
	s.runWorkspaceSetup(ws)

	summary, err := s.workspaces.GetSummary(ctx, ws.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace summary: " + err.Error())
	}
	if summary == nil {
		return nil, httpapi.Internal("workspace summary missing after create")
	}
	response := s.toWorkspaceResponse(ctx, summary)
	response.Created = createdBranch
	return &createWorkspaceOutput{
		Status: http.StatusAccepted,
		Body:   response,
	}, nil
}

// adHocWorkspaceForBranch returns the accepted response for an ad-hoc
// workspace that already owns the requested branch, or nil when none exists.
func (s *Handler) adHocWorkspaceForBranch(
	ctx context.Context, repo *db.Repo, itemKey string,
) (*createWorkspaceOutput, error) {
	existing, err := s.workspaces.GetByItemKeyForProvider(
		ctx,
		repo.Platform,
		repo.PlatformHost,
		repo.Owner,
		repo.Name,
		db.WorkspaceItemTypeAdHoc,
		itemKey,
	)
	if err != nil {
		return nil, httpapi.Internal("lookup existing workspace: " + err.Error())
	}
	if existing == nil {
		return nil, nil
	}
	summary, err := s.workspaces.GetSummary(ctx, existing.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace summary: " + err.Error())
	}
	if summary == nil {
		return nil, httpapi.Internal("workspace summary missing for existing workspace")
	}
	return &createWorkspaceOutput{
		Status: http.StatusAccepted,
		Body:   s.toWorkspaceResponse(ctx, summary),
	}, nil
}

// repositoryRouteFenceProblem fails workspace creation closed when repository
// reconciliation reassigned the request's validated route mid-flight. The
// fence-guarded write layer reports this before any row is persisted for the
// replacement repository.
func repositoryRouteFenceProblem(err error) error {
	if errors.Is(err, db.ErrRepositoryRouteFenceChanged) {
		return httpapi.NotFound(httpapi.CodeRepoNotFound,
			"repository identity changed during workspace creation", nil)
	}
	return nil
}

func (s *Handler) adHocWorkspaceCreateError(
	ctx context.Context, repo *db.Repo, itemKey string, err error,
) (*createWorkspaceOutput, error) {
	if problem := repositoryRouteFenceProblem(err); problem != nil {
		return nil, problem
	}
	msg := err.Error()
	if branchConflict, ok := errors.AsType[*workspace.WorkspaceBranchConflictError](err); ok {
		conflict := httpapi.NewProblem(
			http.StatusConflict,
			httpapi.CodeBranchConflict,
			"A local branch with the requested name already exists.",
			map[string]any{
				"branch":          branchConflict.Branch,
				"suggestedBranch": branchConflict.SuggestedBranch,
			},
		)
		conflict.Type = adHocWorkspaceBranchConflictType
		conflict.Title = "Workspace branch conflict"
		return nil, conflict
	}
	if errors.Is(err, workspace.ErrWorkspaceNotFound) {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, msg, nil)
	}
	if errors.Is(err, workspace.ErrInvalidBranchName) {
		return nil, httpapi.Validation("body.branch", msg)
	}
	// A racing create for the same branch loses on the workspace unique
	// index; hand back the winner instead of a conflict the caller cannot
	// act on.
	if itemKey != "" && errors.Is(err, workspace.ErrWorkspaceDuplicate) {
		existing, existingErr := s.adHocWorkspaceForBranch(ctx, repo, itemKey)
		if existingErr == nil && existing != nil {
			return existing, nil
		}
		return nil, httpapi.Conflict(httpapi.CodeConflict,
			"workspace already exists for this branch", nil)
	}
	return nil, httpapi.Internal("create workspace: " + msg)
}

// CreateAdHocWorkspace creates or reuses a workspace for new work.
func (s *Handler) CreateAdHocWorkspace(
	ctx context.Context, input *CreateAdHocWorkspaceInput,
) (*CreateWorkspaceOutput, error) {
	return s.createAdHocWorkspace(ctx, input)
}

// listWorkspaces returns kenn-forge's persisted workspace records.
//
// Its purpose is to drive the workspaces page and terminal picker from
// kenn-forge's own database model, rather than from ad hoc discovery of host
// worktrees.
func (s *Handler) listWorkspaces(
	ctx context.Context, _ *struct{},
) (*listWorkspacesOutput, error) {
	if s.workspaces == nil {
		out := &listWorkspacesOutput{}
		out.Body.Workspaces = []workspaceResponse{}
		return out, nil
	}

	s.scheduleWorkspaceTmuxPrune()

	summaries, err := s.workspaces.ListSummaries(ctx)
	if err != nil {
		return nil, httpapi.Internal("list workspaces failed")
	}
	s.trimWorkspaceEnrichmentCache(summaries)

	list := make([]workspaceResponse, len(summaries))
	for i := range summaries {
		list[i] = s.toCachedWorkspaceResponse(&summaries[i])
	}

	out := &listWorkspacesOutput{}
	out.Body.Workspaces = list
	return out, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// getWorkspace returns one persisted kenn-forge workspace.
//
// The terminal view uses this to reopen an existing local execution context and
// determine whether the workspace is PR-backed or issue-backed.
func (s *Handler) getWorkspace(
	ctx context.Context, input *getWorkspaceInput,
) (*getWorkspaceOutput, error) {
	result, err := s.GetWorkspaceService(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	return &getWorkspaceOutput{Body: result.Workspace}, nil
}

func (s *Handler) GetWorkspaceService(ctx context.Context, id string) (WorkspaceResult, error) {
	if s.workspaces == nil {
		return WorkspaceResult{}, httpapi.ServiceUnavailable("workspace manager not configured")
	}
	summary, err := s.workspaces.GetSummary(ctx, id)
	if err != nil {
		return WorkspaceResult{}, httpapi.Internal("get workspace failed")
	}
	if summary == nil {
		return WorkspaceResult{}, httpapi.NotFound(
			httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
		)
	}
	return WorkspaceResult{Workspace: s.toCachedWorkspaceResponse(summary)}, nil
}

func (s *Handler) refreshWorkspace(
	ctx context.Context, input *refreshWorkspaceInput,
) (*refreshWorkspaceOutput, error) {
	if s.workspaces == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}

	summary, err := s.workspaces.GetSummary(ctx, input.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace failed")
	}
	if summary == nil {
		return nil, httpapi.NotFound(
			httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
		)
	}
	if s.workspaceDiffCache != nil {
		s.workspaceDiffCache.RevalidateWorkspace(input.ID)
	}
	if s.syncer == nil {
		workspace := summary.Workspace
		if _, err := s.workspaces.RefreshWorkspaceLaunchSpec(ctx, &workspace); err != nil {
			return nil, workspaceLaunchSpecProblem(err)
		}
		refreshed, err := s.workspaces.GetSummary(ctx, input.ID)
		if err != nil {
			return nil, httpapi.Internal("get workspace failed")
		}
		if refreshed == nil {
			return nil, httpapi.NotFound(
				httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
			)
		}
		resp := s.refreshWorkspaceResponse(ctx, refreshed)
		s.hub.Broadcast(Event{Type: "workspace_status", Data: resp})
		s.hub.Broadcast(Event{Type: "data_changed", Data: struct{}{}})
		return &refreshWorkspaceOutput{Body: resp}, nil
	}

	provider := strings.TrimSpace(summary.Platform)
	if provider == "" {
		provider = string(platform.KindGitHub)
	}
	repo, err := s.lookupRepoByProviderRoute(
		ctx, provider, summary.PlatformHost, summary.RepoOwner, summary.RepoName,
	)
	if err != nil {
		return nil, providerRouteLookupError(err)
	}
	kind := repoProviderKind(*repo)
	host := repoProviderHost(*repo)

	switch summary.ItemType {
	case db.WorkspaceItemTypeIssue:
		if err := s.refreshWorkspaceIssue(
			ctx, repo.ID, kind, host, repo.Owner, repo.Name, summary.ItemNumber,
		); err != nil {
			return nil, err
		}
	case db.WorkspaceItemTypePullRequest:
		// The PR detail sync runs after the repo index refresh below so the
		// workspace response reflects the latest indexed PR row and diff.
	case db.WorkspaceItemTypeKataTask:
		// Kata tasks are not provider issues. The live task pane refreshes
		// through the Kata daemon; this route only refreshes the mapped repo.
	case db.WorkspaceItemTypeAdHoc:
		// Ad-hoc workspaces have no source item to refresh. Only the mapped
		// repo index and any PR later detected for the branch matter.
	default:
		return nil, httpapi.Internal("workspace has unsupported item type")
	}

	if err := s.refreshWorkspaceRepoIndex(
		ctx, kind, host, repo.Owner, repo.Name,
	); err != nil {
		return nil, err
	}

	if s.workspacePRMonitor != nil {
		update, changed, err := s.workspacePRMonitor.RefreshWorkspaceAssociation(
			ctx, input.ID,
		)
		if err != nil {
			return nil, httpapi.Internal("refresh workspace PR association: " + err.Error())
		}
		if changed {
			s.broadcastWorkspaceStatus(update.WorkspaceID)
			s.hub.Broadcast(Event{Type: "data_changed", Data: struct{}{}})
		}
	}

	refreshed, err := s.workspaces.GetSummary(ctx, input.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace failed")
	}
	if refreshed == nil {
		return nil, httpapi.NotFound(
			httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
		)
	}
	if prNumber, ok := workspaceAssociatedPRNumber(refreshed); ok {
		if err := s.refreshWorkspacePullRequest(
			ctx, repo.ID, kind, host, repo.Owner, repo.Name, prNumber,
		); err != nil {
			return nil, err
		}
		refreshed, err = s.workspaces.GetSummary(ctx, input.ID)
		if err != nil {
			return nil, httpapi.Internal("get workspace failed")
		}
		if refreshed == nil {
			return nil, httpapi.NotFound(
				httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
			)
		}
	}

	resp := s.refreshWorkspaceResponse(ctx, refreshed)
	s.hub.Broadcast(Event{Type: "workspace_status", Data: resp})
	s.hub.Broadcast(Event{Type: "data_changed", Data: struct{}{}})
	return &refreshWorkspaceOutput{Body: resp}, nil
}

func workspaceAssociatedPRNumber(summary *db.WorkspaceSummary) (int, bool) {
	if summary == nil {
		return 0, false
	}
	if summary.ItemType == db.WorkspaceItemTypePullRequest {
		return summary.ItemNumber, summary.ItemNumber > 0
	}
	if summary.AssociatedPRNumber == nil {
		return 0, false
	}
	return *summary.AssociatedPRNumber, *summary.AssociatedPRNumber > 0
}

func (s *Handler) refreshWorkspaceRepoIndex(
	ctx context.Context,
	kind platform.Kind,
	host, owner, name string,
) error {
	err := s.syncer.SyncRepoOnProvider(ctx, kind, host, owner, name)
	if err == nil {
		return nil
	}
	// An issue-only partial failure is already recorded in repo sync
	// health, and the workspace flow depends on merge-request data, so the
	// refresh proceeds to association inspection and the targeted
	// PR-detail refresh. Partial failures touching the merge-request
	// scope (and hard failures) still abort: succeeding would return
	// stale association data for a PR that failed to sync.
	if partial, ok := ghclient.ExclusivePartialSyncFailure(err); ok && !partial.MergeRequests {
		slog.Warn("workspace refresh: issue sync partially failed",
			"owner", owner, "name", name, "err", err)
		return nil
	}
	if strings.Contains(err.Error(), "is not tracked") {
		return httpapi.Forbidden(err.Error(), nil)
	}
	return httpapi.ProviderCallProblemWithDetail(
		err, string(kind), host, "sync repo: "+err.Error(),
	)
}

func (s *Handler) refreshWorkspaceIssue(
	ctx context.Context,
	repoID int64,
	kind platform.Kind,
	host, owner, name string,
	number int,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeIssue, number,
	)
	if err != nil {
		return httpapi.Internal("check issue visibility: " + err.Error())
	}
	if removed {
		return nil
	}
	err = s.syncer.SyncIssueOnProvider(ctx, kind, host, owner, name, number)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "is not tracked") {
		return httpapi.Forbidden(err.Error(), nil)
	}
	return httpapi.ProviderCallProblemWithDetail(
		err, string(kind), host, "sync issue: "+err.Error(),
	)
}

func (s *Handler) refreshWorkspacePullRequest(
	ctx context.Context,
	repoID int64,
	kind platform.Kind,
	host, owner, name string,
	number int,
) error {
	removed, err := s.db.IsArchiveItemRemovedUpstream(
		ctx, repoID, db.ArchiveItemTypeMergeRequest, number,
	)
	if err != nil {
		return httpapi.Internal("check pull request visibility: " + err.Error())
	}
	if removed {
		return nil
	}
	var diffErr *ghclient.DiffSyncError
	err = s.syncer.SyncMROnProvider(ctx, kind, host, owner, name, number)
	if err != nil && !errors.As(err, &diffErr) {
		if strings.Contains(err.Error(), "is not tracked") {
			return httpapi.Forbidden(err.Error(), nil)
		}
		return httpapi.ProviderCallProblemWithDetail(
			err, string(kind), host, "sync PR: "+err.Error(),
		)
	}
	if diffErr != nil {
		slog.Warn("diff sync failed during workspace refresh",
			"owner", owner,
			"name", name,
			"number", number,
			"code", diffErr.Code,
			"err", diffErr.Err,
		)
	}
	return nil
}

func (s *Handler) getWorkspaceCommits(
	ctx context.Context, input *getWorkspaceCommitsInput,
) (*getWorkspaceCommitsOutput, error) {
	req, err := s.workspaceDiffRequest(ctx, input.ID, "")
	if err != nil {
		return nil, err
	}

	commits, ok, err := s.workspaceCommits(ctx, req)
	if err != nil {
		slog.Error(
			"failed to list workspace commits",
			"workspace_id", input.ID,
			"err", err,
		)
		return nil, httpapi.Upstream("failed to list workspace commits", "", "")
	}
	if !ok {
		return nil, httpapi.NotFound(httpapi.CodeNotFound,
			"commits not available for this workspace", nil)
	}

	// Annotate each commit with whether it has reached the branch's upstream so
	// the UI can flag local-only commits. Push status is an enhancement over the
	// commit list, so a probe failure degrades to omitting the flag rather than
	// failing the request. When the branch has no upstream we cannot tell pushed
	// from unpushed (a fork PR head has no upstream yet already exists on its
	// remote), so the flag is omitted rather than guessed.
	unpushed, hasUpstream, pushErr := workspace.WorktreeUnpushedSHAs(
		ctx, req.Summary.WorktreePath,
	)
	if pushErr != nil {
		slog.Warn(
			"failed to determine unpushed workspace commits",
			"workspace_id", input.ID,
			"err", pushErr,
		)
	}

	resp := commitsResponse{Commits: make([]commitResponse, len(commits))}
	for i, c := range commits {
		cr := commitResponse{
			SHA:        c.SHA,
			Message:    c.Message,
			AuthorName: c.AuthorName,
			AuthoredAt: c.AuthoredAt.UTC(),
		}
		if pushErr == nil && hasUpstream {
			_, isUnpushed := unpushed[c.SHA]
			pushed := !isUnpushed
			cr.Pushed = &pushed
		}
		resp.Commits[i] = cr
	}
	return &getWorkspaceCommitsOutput{Body: resp}, nil
}

func (s *Handler) getWorkspaceFiles(
	ctx context.Context, input *getWorkspaceFilesInput,
) (*getWorkspaceFilesOutput, error) {
	req, err := s.workspaceDiffRequest(ctx, input.ID, input.Base)
	if err != nil {
		return nil, err
	}
	if err := s.applyWorkspaceDiffScope(
		ctx, &req, input.Commit, input.From, input.To,
	); err != nil {
		return nil, err
	}

	hideWhitespace := input.Whitespace == "hide"
	snapshot, _, diffErr := s.workspaceDiffCache.Get(
		ctx, s.workspaceDiffCacheKey(req, hideWhitespace),
	)
	if diffErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(diffErr, errWorkspaceDiffBaseUnavailable) {
			return nil, workspaceDiffBaseUnavailable(req.Base)
		}
		slog.Error(
			"failed to list workspace diff files",
			"workspace_id", input.ID,
			"base", req.Base,
			"err", diffErr,
		)
		return nil, httpapi.Upstream("failed to list workspace files", "", "")
	}
	return &getWorkspaceFilesOutput{Body: filesResponse{
		Stale:               snapshot.Diff.Stale,
		WhitespaceOnlyCount: snapshot.Diff.WhitespaceOnlyCount,
		Files:               snapshot.Files,
		SnapshotVersion:     snapshot.Version,
	}}, nil
}

const workspaceDiffWatchTimeout = 25 * time.Second

func (s *Handler) watchWorkspaceDiff(
	ctx context.Context,
	input *watchWorkspaceDiffInput,
) (*watchWorkspaceDiffOutput, error) {
	req, err := s.workspaceDiffRequest(ctx, input.ID, string(workspace.WorktreeDiffBaseHead))
	if err != nil {
		return nil, err
	}
	key := s.workspaceDiffCacheKey(req, false)
	events, hubDone := s.hub.Subscribe(ctx, false)
	releaseSelection := s.workspaceDiffCache.Select(
		input.ID,
		func(context.Context) (workspaceDiffLogicalKey, error) {
			return key, nil
		},
	)
	defer releaseSelection()

	snapshot, _, err := s.workspaceDiffCache.Get(ctx, key)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, errWorkspaceDiffBaseUnavailable) {
			return nil, workspaceDiffBaseUnavailable(req.Base)
		}
		return nil, httpapi.Upstream("failed to prepare selected workspace diff", "", "")
	}
	if input.Version == "" || input.Version != snapshot.Version {
		return &watchWorkspaceDiffOutput{Body: workspaceDiffWatchResponse{
			Changed: true,
			Version: snapshot.Version,
		}}, nil
	}

	timer := time.NewTimer(workspaceDiffWatchTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-hubDone:
			return &watchWorkspaceDiffOutput{Body: workspaceDiffWatchResponse{
				Version: snapshot.Version,
			}}, nil
		case <-timer.C:
			return &watchWorkspaceDiffOutput{Body: workspaceDiffWatchResponse{
				Version: snapshot.Version,
			}}, nil
		case event, ok := <-events:
			if !ok {
				return &watchWorkspaceDiffOutput{Body: workspaceDiffWatchResponse{
					Version: snapshot.Version,
				}}, nil
			}
			if event.Type != "workspace_diff_ready" && event.Type != "workspace_diff_changed" {
				continue
			}
			data, ok := event.Data.(workspaceDiffEventData)
			if !ok || data.WorkspaceID != input.ID {
				continue
			}
			current, _, getErr := s.workspaceDiffCache.Get(ctx, key)
			if getErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				return nil, httpapi.Upstream("failed to read selected workspace diff", "", "")
			}
			if current.Version == input.Version {
				continue
			}
			return &watchWorkspaceDiffOutput{Body: workspaceDiffWatchResponse{
				Changed: true,
				Version: current.Version,
			}}, nil
		}
	}
}

func (s *Handler) getWorkspaceDiff(
	ctx context.Context, input *getWorkspaceDiffInput,
) (*getWorkspaceDiffOutput, error) {
	req, err := s.workspaceDiffRequest(ctx, input.ID, input.Base)
	if err != nil {
		return nil, err
	}
	if err := s.applyWorkspaceDiffScope(
		ctx, &req, input.Commit, input.From, input.To,
	); err != nil {
		return nil, err
	}

	hideWhitespace := input.Whitespace == "hide"
	snapshot, _, diffErr := s.workspaceDiffCache.Get(
		ctx, s.workspaceDiffCacheKey(req, hideWhitespace),
	)
	if diffErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(diffErr, errWorkspaceDiffBaseUnavailable) {
			return nil, workspaceDiffBaseUnavailable(req.Base)
		}
		slog.Error(
			"failed to compute workspace diff",
			"workspace_id", input.ID,
			"base", req.Base,
			"err", diffErr,
		)
		return nil, httpapi.Upstream("failed to compute workspace diff", "", "")
	}
	if input.Revision != "" && input.Revision != snapshot.Version {
		return nil, httpapi.Conflict(
			httpapi.CodeConflict,
			"workspace diff snapshot changed; reload the file list",
			map[string]any{"reason": "snapshot_changed", "snapshot_version": snapshot.Version},
		)
	}
	files := snapshot.Diff.Files
	if input.Path != "" {
		files = filterWorkspaceDiffSnapshotPath(files, input.Path)
	}
	return &getWorkspaceDiffOutput{Body: diffResponse{
		Stale:               snapshot.Diff.Stale,
		WhitespaceOnlyCount: snapshot.Diff.WhitespaceOnlyCount,
		Files:               files,
		SnapshotVersion:     snapshot.Version,
	}}, nil
}

func (s *Handler) getWorkspaceFilePreview(
	ctx context.Context,
	input *getWorkspaceFilePreviewInput,
) (*getWorkspaceFilePreviewOutput, error) {
	if strings.TrimSpace(input.Path) == "" {
		return nil, httpapi.Validation("query.path", "path is required")
	}
	side := strings.TrimSpace(input.Side)
	if side != "" && side != "old" && side != "new" {
		return nil, httpapi.Validation("query.side", "side must be old or new")
	}

	req, err := s.workspaceDiffRequest(ctx, input.ID, input.Base)
	if err != nil {
		return nil, err
	}
	if err := s.applyWorkspaceDiffScope(
		ctx, &req, input.Commit, input.From, input.To,
	); err != nil {
		return nil, err
	}

	hideWhitespace := input.Whitespace == "hide"
	snapshot, _, err := s.workspaceDiffCache.Get(
		ctx, s.workspaceDiffCacheKey(req, hideWhitespace),
	)
	var content *gitclone.FileContent
	if err == nil {
		if input.Revision != "" && input.Revision != snapshot.Version {
			return nil, httpapi.Conflict(
				httpapi.CodeConflict,
				"workspace diff snapshot changed; reload the file list",
				map[string]any{"reason": "snapshot_changed", "snapshot_version": snapshot.Version},
			)
		}
		matching := filterWorkspaceDiffSnapshotPath(snapshot.Diff.Files, input.Path)
		if len(matching) == 0 {
			err = gitclone.ErrNotFound
		} else {
			content, err = workspace.ReadDiffSnapshotFile(
				ctx, snapshot.Resolved, matching[0], side, maxFilePreviewBytes,
			)
		}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, errWorkspaceDiffBaseUnavailable) {
			return nil, workspaceDiffBaseUnavailable(req.Base)
		}
		if errors.Is(err, gitclone.ErrNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeNotFound, "workspace file preview not available: file is not changed in this diff", nil)
		}
		if errors.Is(err, gitclone.ErrTooLarge) {
			return nil, httpapi.PayloadTooLarge("file preview is too large", maxFilePreviewBytes)
		}
		slog.Error(
			"failed to read workspace file preview",
			"workspace_id", input.ID,
			"base", req.Base,
			"path", input.Path,
			"err", err,
		)
		return nil, httpapi.Upstream("failed to read workspace file preview", "", "")
	}
	if content == nil {
		return nil, httpapi.Upstream("failed to read workspace file preview", "", "")
	}
	return &getWorkspaceFilePreviewOutput{Body: filePreviewResponse{
		Path:      content.Path,
		MediaType: previewMediaType(content.Path, content.Data),
		Encoding:  "base64",
		Content:   base64.StdEncoding.EncodeToString(content.Data),
		Size:      content.Size,
	}}, nil
}

func (s *Handler) workspaceDiffRequest(
	ctx context.Context,
	id string,
	baseInput string,
) (workspaceDiffRequest, error) {
	if s.workspaces == nil {
		return workspaceDiffRequest{}, httpapi.ServiceUnavailable(
			"workspace manager not configured",
		)
	}

	summary, err := s.workspaces.GetSummary(ctx, id)
	if err != nil {
		return workspaceDiffRequest{}, httpapi.Internal("get workspace failed")
	}
	if summary == nil {
		return workspaceDiffRequest{}, httpapi.NotFound(
			httpapi.CodeWorkspaceNotFound, "workspace not found", nil,
		)
	}
	if summary.Status != "ready" {
		return workspaceDiffRequest{}, httpapi.Conflict(
			httpapi.CodeConflict, "workspace is not ready", nil,
		)
	}

	base := workspace.WorktreeDiffBase(baseInput)
	if base == "" {
		base = workspace.WorktreeDiffBaseHead
	}
	switch base {
	case workspace.WorktreeDiffBaseHead, workspace.WorktreeDiffBasePushed:
		return workspaceDiffRequest{Summary: summary, Base: base}, nil
	case workspace.WorktreeDiffBaseMergeTarget:
		targetBranch, ok, err := s.workspaceMergeTargetBranch(ctx, summary)
		if err != nil {
			return workspaceDiffRequest{}, err
		}
		if !ok {
			return workspaceDiffRequest{}, workspaceDiffBaseUnavailable(base)
		}
		return workspaceDiffRequest{
			Summary:           summary,
			Base:              base,
			MergeTargetBranch: targetBranch,
		}, nil
	default:
		return workspaceDiffRequest{}, httpapi.Validation(
			"query.base",
			"base must be head, pushed, or merge-target",
			"head", "pushed", "merge-target",
		)
	}
}

func (s *Handler) workspaceDiffCacheKey(
	req workspaceDiffRequest,
	hideWhitespace bool,
) workspaceDiffLogicalKey {
	return workspaceDiffLogicalKey{
		WorkspaceID: req.Summary.ID,
		Spec: workspace.DiffSnapshotSpec{
			WorktreePath:      req.Summary.WorktreePath,
			Base:              req.Base,
			MergeTargetBranch: req.MergeTargetBranch,
			FromSHA:           req.FromSHA,
			ToSHA:             req.ToSHA,
			HideWhitespace:    hideWhitespace,
		},
	}
}

// SelectWorkspaceDiff holds the selected-workspace refresh lease used by the
// root SSE stream until the returned release function is called.
func (s *Handler) SelectWorkspaceDiff(workspaceID string) func() {
	if s == nil || s.workspaceDiffCache == nil || workspaceID == "" {
		return func() {}
	}
	return s.workspaceDiffCache.Select(
		workspaceID,
		func(ctx context.Context) (workspaceDiffLogicalKey, error) {
			req, err := s.workspaceDiffRequest(
				ctx, workspaceID, string(workspace.WorktreeDiffBaseHead),
			)
			if err != nil {
				return workspaceDiffLogicalKey{}, err
			}
			return s.workspaceDiffCacheKey(req, false), nil
		},
	)
}

func filterWorkspaceDiffSnapshotPath(
	files []gitclone.DiffFile,
	path string,
) []gitclone.DiffFile {
	for i := range files {
		if files[i].Path == path {
			return []gitclone.DiffFile{files[i]}
		}
	}
	for i := range files {
		if files[i].OldPath == path {
			return []gitclone.DiffFile{files[i]}
		}
	}
	return []gitclone.DiffFile{}
}

func (s *Handler) workspaceCommits(
	ctx context.Context,
	req workspaceDiffRequest,
) ([]gitclone.Commit, bool, error) {
	targetBranch, ok, err := s.workspaceMergeTargetBranch(ctx, req.Summary)
	if err != nil || !ok {
		return nil, ok, err
	}
	return workspace.WorktreeCommitsAgainstMergeTarget(
		ctx,
		req.Summary.WorktreePath,
		targetBranch,
	)
}

func (s *Handler) applyWorkspaceDiffScope(
	ctx context.Context,
	req *workspaceDiffRequest,
	commit string,
	from string,
	to string,
) error {
	hasCommit := commit != ""
	hasFrom := from != ""
	hasTo := to != ""

	switch {
	case !hasCommit && !hasFrom && !hasTo:
		return nil

	case hasCommit && !hasFrom && !hasTo:
		if _, err := s.validateWorkspaceSHAs(ctx, *req, commit); err != nil {
			return err
		}
		parent, err := workspace.WorktreeParentOf(
			ctx, req.Summary.WorktreePath, commit,
		)
		if err != nil {
			return httpapi.Internal("failed to resolve parent: " + err.Error())
		}
		req.FromSHA = parent
		req.ToSHA = commit
		return nil

	case !hasCommit && hasFrom && hasTo:
		indexMap, err := s.validateWorkspaceSHAs(ctx, *req, from, to)
		if err != nil {
			return err
		}
		if indexMap[from] < indexMap[to] {
			return httpapi.Validation("query",
				"invalid range: 'from' must be older than or equal to 'to'",
			)
		}
		parent, err := workspace.WorktreeParentOf(
			ctx, req.Summary.WorktreePath, from,
		)
		if err != nil {
			return httpapi.Internal("failed to resolve parent: " + err.Error())
		}
		req.FromSHA = parent
		req.ToSHA = to
		return nil

	default:
		return httpapi.Validation("query",
			"invalid scope: use 'commit' alone or 'from'+'to' together",
		)
	}
}

func (s *Handler) validateWorkspaceSHAs(
	ctx context.Context,
	req workspaceDiffRequest,
	shas ...string,
) (map[string]int, error) {
	commits, ok, err := s.workspaceCommits(ctx, req)
	if err != nil {
		return nil, httpapi.Upstream(
			"failed to list workspace commits: "+err.Error(), "", "",
		)
	}
	if !ok {
		return nil, httpapi.NotFound(httpapi.CodeNotFound,
			"commits not available for this workspace", nil)
	}
	indexMap := make(map[string]int, len(commits))
	for i, c := range commits {
		indexMap[c.SHA] = i
	}
	for _, sha := range shas {
		if _, ok := indexMap[sha]; !ok {
			return nil, httpapi.Validation("query",
				"invalid scope: commit is not in this workspace branch",
			)
		}
	}
	return indexMap, nil
}

func (s *Handler) workspaceMergeTargetBranch(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) (string, bool, error) {
	prNumber := summary.ItemNumber
	if summary.ItemType != db.WorkspaceItemTypePullRequest {
		if summary.AssociatedPRNumber == nil {
			return "", false, nil
		}
		prNumber = *summary.AssociatedPRNumber
	}
	if prNumber <= 0 {
		return "", false, nil
	}

	repo, err := s.db.GetRepoByIdentity(ctx, db.RepoIdentity{
		Platform:     summary.Platform,
		PlatformHost: summary.PlatformHost,
		Owner:        summary.RepoOwner,
		Name:         summary.RepoName,
	})
	if err != nil {
		return "", false, httpapi.Internal("get workspace repo failed")
	}
	if repo == nil {
		return "", false, nil
	}

	mr, err := s.db.GetVisibleMergeRequestByRepoIDAndNumber(
		ctx, repo.ID, prNumber,
	)
	if err != nil {
		return "", false, httpapi.Internal("get workspace pull request failed")
	}
	if mr == nil || strings.TrimSpace(mr.BaseBranch) == "" {
		return "", false, nil
	}
	return strings.TrimSpace(mr.BaseBranch), true, nil
}

func workspaceDiffBaseUnavailable(
	base workspace.WorktreeDiffBase,
) error {
	if base == workspace.WorktreeDiffBaseMergeTarget {
		return httpapi.NotFound(httpapi.CodeNotFound,
			"workspace merge target branch not available", nil)
	}
	return httpapi.NotFound(httpapi.CodeNotFound,
		"workspace pushed branch not available", nil)
}

func (s *Handler) retryWorkspace(
	ctx context.Context, input *retryWorkspaceInput,
) (*createWorkspaceOutput, error) {
	if s.workspaces == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}

	ws, startNow, err := s.workspaces.RequestRetry(ctx, input.ID)
	if err != nil {
		if errors.Is(err, workspace.ErrWorkspaceNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceInvalidState) {
			return nil, httpapi.Conflict(httpapi.CodeConflict, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceOwnershipUnproven) {
			return nil, httpapi.Conflict(httpapi.CodeConflict, err.Error(), nil)
		}
		return nil, httpapi.Internal("retry workspace: " + err.Error())
	}
	s.invalidateWorkspaceEnrichment(ws.ID)

	if startNow {
		s.runWorkspaceSetup(ws)
	}

	summary, err := s.workspaces.GetSummary(ctx, ws.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace summary: " + err.Error())
	}
	if summary == nil {
		return nil, httpapi.Internal("workspace summary missing after retry")
	}
	resp := s.toWorkspaceResponse(ctx, summary)
	s.hub.Broadcast(Event{
		Type: "workspace_status",
		Data: resp,
	})
	return &createWorkspaceOutput{
		Status: http.StatusAccepted,
		Body:   resp,
	}, nil
}

func (s *Handler) toWorkspaceResponse(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) workspaceResponse {
	resp := s.workspaceResponseWithEnrichment(ctx, summary).response
	s.applyAgentActivity(&resp, summary)
	return resp
}

func (s *Handler) applyAgentActivity(
	resp *workspaceResponse,
	summary *db.WorkspaceSummary,
) {
	if s.agentActivity == nil || s.runtime == nil || resp == nil || summary == nil {
		return
	}
	snapshot, ok := s.agentActivity.SnapshotForWorkspace(
		summary.WorktreePath, s.liveAgentSessionKeys(summary.ID),
	)
	if !ok {
		return
	}
	state := string(snapshot.State)
	updatedAt := formatAgentActivityUpdatedAt(snapshot.UpdatedAt)
	resp.AgentState = &state
	resp.AgentStateUpdatedAt = &updatedAt
}

func formatAgentActivityUpdatedAt(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// Response returns the cached public DTO for a persisted workspace summary.
func (s *Handler) Response(
	ctx context.Context, summary *db.WorkspaceSummary,
) WorkspaceResponse {
	return s.toWorkspaceResponse(ctx, summary)
}

// workspaceEnrichmentProbePlan selects which enrichment components a probe
// runs. gitFingerprint carries the git-directory fingerprint captured before
// the probe so a change during the probe is noticed on the next refresh.
type workspaceEnrichmentProbePlan struct {
	git            bool
	tmux           bool
	gitFingerprint string
}

// workspaceResponseWithEnrichment runs every enrichment component
// unconditionally. Explicit refreshes use it; background refreshes go through
// workspaceResponseWithChangeAwareEnrichment.
func (s *Handler) workspaceResponseWithEnrichment(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) workspaceEnrichmentProbeResult {
	plan := workspaceEnrichmentProbePlan{git: true, tmux: true}
	if summary.Status == "ready" {
		plan.gitFingerprint = s.workspaceGitFingerprint(summary)
	}
	return s.probeWorkspaceEnrichment(ctx, summary, plan)
}

// workspaceResponseWithChangeAwareEnrichment is the background full-refresh
// probe. It spawns git only when the git-directory fingerprint moved since
// the cached probe or the forced interval elapsed, and probes tmux only when
// the tmux component is past its own cadence.
func (s *Handler) workspaceResponseWithChangeAwareEnrichment(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) workspaceEnrichmentProbeResult {
	plan := workspaceEnrichmentProbePlan{git: true, tmux: true}
	if s.workspaces == nil || summary.Status != "ready" {
		return s.probeWorkspaceEnrichment(ctx, summary, plan)
	}
	entry, _ := s.cachedWorkspaceEnrichment(summary.ID, workspaceEnrichmentFull)
	if entry == nil {
		plan.gitFingerprint = s.workspaceGitFingerprint(summary)
		return s.probeWorkspaceEnrichment(ctx, summary, plan)
	}
	now := s.now()
	tmuxDue, divergenceDue := entry.componentsDue(now)
	plan.tmux = tmuxDue
	if !divergenceDue {
		// A full job that was upgraded from tmux-only work must not touch
		// the divergence component before its own cadence elapses.
		plan.git = false
		result := s.probeWorkspaceEnrichment(ctx, summary, plan)
		result.divergenceUnchanged = false
		result.divergenceSkipped = true
		return result
	}
	plan.gitFingerprint = s.workspaceGitFingerprint(summary)
	plan.git = !entry.gitProbeSkippable(plan.gitFingerprint, now)
	return s.probeWorkspaceEnrichment(ctx, summary, plan)
}

func (s *Handler) workspaceGitFingerprint(summary *db.WorkspaceSummary) string {
	fingerprint, err := workspace.WorktreeGitFingerprint(summary.WorktreePath)
	if err != nil {
		slog.Debug(
			"worktree git fingerprint failed",
			"workspace_id", summary.ID,
			"path", summary.WorktreePath,
			"err", err,
		)
		return ""
	}
	return fingerprint
}

func (s *Handler) probeWorkspaceEnrichment(
	ctx context.Context,
	summary *db.WorkspaceSummary,
	plan workspaceEnrichmentProbePlan,
) workspaceEnrichmentProbeResult {
	resp := toWorkspaceResponse(summary)
	resp.Repo = s.repoRefFromParts(
		summary.Platform, summary.PlatformHost, summary.RepoOwner, summary.RepoName,
	)
	if s.workspaces == nil ||
		summary.Status != "ready" {
		return workspaceEnrichmentProbeResult{response: resp}
	}

	result := workspaceEnrichmentProbeResult{
		gitFingerprint:      plan.gitFingerprint,
		divergenceUnchanged: !plan.git,
		tmuxSkipped:         !plan.tmux,
	}
	var gitStateErr error
	if plan.git {
		divergenceErr := applyWorktreeDivergence(ctx, &resp, summary.WorktreePath)
		dirtyErr := applyWorktreeDirty(ctx, &resp, summary.WorktreePath)
		gitStateErr = errors.Join(divergenceErr, dirtyErr)
		result.divergenceComplete = gitStateErr == nil
		result.divergenceErr = gitStateErr
	}
	var tmuxErr error
	if plan.tmux {
		tmuxErr = s.applyWorkspaceTmuxEnrichment(ctx, summary, &resp)
		result.tmuxComplete = tmuxErr == nil
		result.tmuxErr = tmuxErr
	}
	err := errors.Join(gitStateErr, tmuxErr)
	result.err = err
	if err != nil {
		resp.EnrichmentStatus = workspaceEnrichmentFailed
		message := err.Error()
		resp.EnrichmentError = &message
		result.response = resp
		return result
	}
	resp.EnrichmentStatus = workspaceEnrichmentFresh
	refreshedAt := s.now().UTC().Format(time.RFC3339)
	resp.EnrichmentRefreshedAt = &refreshedAt
	result.response = resp
	return result
}

func (s *Handler) workspaceResponseWithTmuxEnrichment(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) workspaceEnrichmentProbeResult {
	resp := toWorkspaceResponse(summary)
	resp.Repo = s.repoRefFromParts(
		summary.Platform, summary.PlatformHost, summary.RepoOwner, summary.RepoName,
	)
	if s.workspaces == nil || summary.Status != "ready" {
		return workspaceEnrichmentProbeResult{response: resp, kind: workspaceEnrichmentTmux}
	}
	err := s.applyWorkspaceTmuxEnrichment(ctx, summary, &resp)
	return workspaceEnrichmentProbeResult{
		response: resp, tmuxComplete: err == nil, tmuxErr: err, err: err,
		kind: workspaceEnrichmentTmux,
	}
}

// applyWorkspaceTmuxEnrichment fills the tmux activity fields of resp from a
// pane probe. A workspace whose live agent session has reported through the
// hook integration is not probed at all: the lifecycle events are the
// authoritative activity signal there, and the pane capture would only spend
// tmux spawns to second-guess them. Such a workspace counts as a complete
// tmux sample whose last-activity time is the hook report's own timestamp, so
// subject activity feeds still see hook-driven work move.
func (s *Handler) applyWorkspaceTmuxEnrichment(
	ctx context.Context,
	summary *db.WorkspaceSummary,
	resp *workspaceResponse,
) error {
	if latest, ok := s.latestHookReportForWorkspace(summary); ok {
		activityAt := latest.UpdatedAt.UTC().Format(time.RFC3339)
		resp.TmuxLastOutputAt = &activityAt
		return nil
	}
	sessions, sessionsErr := s.workspaceTmuxActivitySessions(ctx, summary)
	activity, hasActivity, activityErr := s.probeWorkspaceTmuxActivity(ctx, summary, sessions)
	if hasActivity {
		applyTmuxActivity(resp, activity)
	}
	return errors.Join(sessionsErr, activityErr)
}

// latestHookReportForWorkspace returns the most recent hook report among the
// workspace's live agent sessions, when one exists. It is deliberately the
// newest report rather than the state-priority pick behind AgentState: with
// two live agents an older approval must not hide a newer event when the
// workspace's last activity is derived.
func (s *Handler) latestHookReportForWorkspace(
	summary *db.WorkspaceSummary,
) (agentactivity.Report, bool) {
	if s.agentActivity == nil || s.runtime == nil || summary == nil {
		return agentactivity.Report{}, false
	}
	reports := s.agentActivity.LiveReportsForWorkspace(
		summary.WorktreePath, s.liveAgentSessionKeys(summary.ID),
	)
	if len(reports) == 0 {
		return agentactivity.Report{}, false
	}
	return reports[0], true
}

// liveAgentSessionKeys lists the runtime session keys of the workspace's
// running or starting agent sessions, the only sessions whose hook reports
// count.
func (s *Handler) liveAgentSessionKeys(workspaceID string) []string {
	liveSessionKeys := make([]string, 0)
	for _, session := range s.runtime.ListSessions(workspaceID) {
		if session.Kind == localruntime.LaunchTargetAgent &&
			(session.Status == localruntime.SessionStatusRunning ||
				session.Status == localruntime.SessionStatusStarting) {
			liveSessionKeys = append(liveSessionKeys, session.Key)
		}
	}
	return liveSessionKeys
}

func (s *Handler) workspaceTmuxActivitySessions(
	ctx context.Context,
	summary *db.WorkspaceSummary,
) ([]string, error) {
	sessions := make([]string, 0, 1)
	seen := map[string]bool{}
	var listErr error
	if s.workspaces != nil {
		stored, err := s.workspaces.TmuxSessionsForWorkspace(
			ctx, summary.ID, summary.TmuxSession,
		)
		if err != nil {
			listErr = err
			slog.Debug(
				"list workspace tmux sessions",
				"workspace_id", summary.ID,
				"tmux_session", summary.TmuxSession,
				"err", err,
			)
		}
		for _, session := range stored {
			if session == "" || seen[session] {
				continue
			}
			sessions = append(sessions, session)
			seen[session] = true
		}
	}
	if summary.TmuxSession != "" && !seen[summary.TmuxSession] {
		sessions = append(sessions, summary.TmuxSession)
		seen[summary.TmuxSession] = true
	}
	if s.runtime == nil {
		return sessions, listErr
	}
	for _, session := range s.runtime.TmuxSessions(summary.ID) {
		if session == "" || seen[session] {
			continue
		}
		sessions = append(sessions, session)
		seen[session] = true
	}
	return sessions, listErr
}

func (s *Handler) probeWorkspaceTmuxActivity(
	ctx context.Context,
	summary *db.WorkspaceSummary,
	sessions []string,
) (tmuxActivityResult, bool, error) {
	if len(sessions) == 0 {
		return tmuxActivityResult{}, false, nil
	}
	tracker := s.tmuxActivity
	if tracker == nil {
		tracker = newTmuxActivityTracker(nil)
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, tmuxActivityProbeTimeout)
	defer cancelProbe()

	results := make([]tmuxActivityResult, 0, len(sessions))
	var probeErrs []error
	for _, session := range sessions {
		if s.tmuxActivity != nil {
			if result, ok := tracker.Cached(session); ok {
				results = append(results, result)
				continue
			}
		}
		result, ok, err := s.probeOneTmuxSession(
			probeCtx, tracker, summary, session,
		)
		if ok {
			results = append(results, result)
		}
		if err != nil {
			probeErrs = append(probeErrs, err)
		}
	}
	result, ok := mergeTmuxActivityResults(results)
	return result, ok, errors.Join(probeErrs...)
}

func (s *Handler) probeOneTmuxSession(
	ctx context.Context,
	tracker *tmuxActivityTracker,
	summary *db.WorkspaceSummary,
	session string,
) (tmuxActivityResult, bool, error) {
	probe := tracker.StartProbe(ctx, session)
	if !probe.Started {
		if probe.Wait != nil {
			select {
			case <-probe.Wait:
				result, ok := tracker.Cached(session)
				if !ok {
					if probe.HasFallback {
						return probe.Fallback, true,
							errors.New("tmux activity probe produced no sample")
					}
					return tmuxActivityResult{}, false,
						errors.New("tmux activity probe produced no sample")
				}
				return result, true, nil
			case <-ctx.Done():
				if probe.HasFallback {
					return probe.Fallback, true, ctx.Err()
				}
			}
		}
		if probe.HasFallback {
			err := ctx.Err()
			if err == nil {
				err = errors.New("tmux activity probe unavailable")
			}
			return probe.Fallback, true, err
		}
		return tmuxActivityResult{}, false, ctx.Err()
	}

	snapshot, err := s.workspaces.TerminalPaneSnapshot(
		ctx, &summary.Workspace, session,
	)
	if err != nil {
		probe.Probe.Cancel()
		slog.Debug(
			"read tmux pane snapshot",
			"workspace_id", summary.ID,
			"tmux_session", session,
			"err", err,
		)
		if probe.HasFallback {
			return probe.Fallback, true, err
		}
		return tmuxActivityResult{}, false, err
	}

	return probe.Probe.Finish(tmuxActivityObservation{
		PaneTitle: snapshot.Title,
		Output:    snapshot.Output,
		HasOutput: true,
	}), true, nil
}

func applyTmuxActivity(resp *workspaceResponse, activity tmuxActivityResult) {
	if activity.PaneTitle != "" {
		title := activity.PaneTitle
		resp.TmuxPaneTitle = &title
	}
	resp.TmuxWorking = activity.Working
	resp.TmuxActivitySource = activity.Source
	if activity.LastOutputAt != nil {
		lastOutputAt := activity.LastOutputAt.UTC().Format(time.RFC3339)
		resp.TmuxLastOutputAt = &lastOutputAt
	}
}

// worktreeDivergenceTimeout caps how long a single workspace's
// rev-list probe can run before the workspace list response moves
// on. Picked to be small enough that a stalled git won't hold up
// the whole list (probes already run in parallel).
const worktreeDivergenceTimeout = 750 * time.Millisecond

func applyWorktreeDivergence(
	ctx context.Context,
	resp *workspaceResponse,
	worktreePath string,
) error {
	if worktreePath == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, worktreeDivergenceTimeout)
	defer cancel()

	div, ok, err := workspace.WorktreeDivergence(probeCtx, worktreePath)
	if err != nil {
		slog.Debug(
			"worktree divergence probe failed",
			"workspace_id", resp.ID,
			"path", worktreePath,
			"err", err,
		)
		return err
	}
	if !ok {
		missing, missingErr := workspace.WorktreeBranchUpstreamMissing(probeCtx, worktreePath)
		if missingErr != nil {
			return missingErr
		}
		if missing {
			resp.BranchUpstreamMissing = &missing
		}
		return nil
	}
	ahead := div.Ahead
	behind := div.Behind
	resp.CommitsAhead = &ahead
	resp.CommitsBehind = &behind
	return nil
}

func applyWorktreeDirty(
	ctx context.Context,
	resp *workspaceResponse,
	worktreePath string,
) error {
	if worktreePath == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, worktreeDivergenceTimeout)
	defer cancel()

	dirty, err := readOnlyWorktreeIsDirty(probeCtx, worktreePath)
	if err != nil {
		slog.Debug(
			"worktree dirty-state probe failed",
			"workspace_id", resp.ID,
			"path", worktreePath,
			"err", err,
		)
		return err
	}
	resp.WorktreeDirty = &dirty
	return nil
}

func readOnlyWorktreeIsDirty(ctx context.Context, worktreePath string) (bool, error) {
	stdout, stderr, err := gitcmd.New().Run(
		ctx,
		worktreePath,
		nil,
		"--no-optional-locks",
		"status",
		"--porcelain",
		"--untracked-files=all",
		"--ignore-submodules=none",
	)
	if err != nil {
		out := append(stdout, stderr...)
		return false, fmt.Errorf(
			"check worktree dirty state: %w: %s",
			err,
			strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(stdout)) != "", nil
}

func isWorkingTmuxTitle(title string) bool {
	normalized := strings.TrimSpace(title)
	if normalized == "" {
		return false
	}

	for _, frame := range "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏" {
		if strings.HasPrefix(normalized, string(frame)+" ") {
			return true
		}
	}

	return false
}

func (s *Handler) getWorkspaceRuntime(
	ctx context.Context,
	input *getWorkspaceRuntimeInput,
) (*getWorkspaceRuntimeOutput, error) {
	result, err := s.GetWorkspaceRuntimeService(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	return &getWorkspaceRuntimeOutput{Body: workspaceRuntimeResponse(result)}, nil
}

func (s *Handler) GetWorkspaceRuntimeService(
	ctx context.Context, workspaceID string,
) (WorkspaceRuntimeResult, error) {
	summary, err := s.getRuntimeWorkspace(ctx, workspaceID)
	if err != nil {
		return WorkspaceRuntimeResult{}, err
	}
	sessions, err := s.workspaceRuntimeSessions(ctx, summary.ID)
	if err != nil {
		return WorkspaceRuntimeResult{}, httpapi.Internal("list runtime sessions: " + err.Error())
	}
	return WorkspaceRuntimeResult{
		LaunchTargets: s.runtime.LaunchTargets(), Sessions: sessions,
	}, nil
}

func (s *Handler) workspaceRuntimeSessions(
	ctx context.Context,
	workspaceID string,
) ([]localruntime.SessionInfo, error) {
	sessions := s.runtime.ListSessions(workspaceID)
	if s.workspaces == nil {
		return sessions, nil
	}
	stored, err := s.workspaces.RuntimeSessionsForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	return mergeStoredRuntimeSessions(sessions, stored), nil
}

func mergeStoredRuntimeSessions(
	live []localruntime.SessionInfo,
	stored []db.WorkspaceRuntimeSession,
) []localruntime.SessionInfo {
	sessions := slices.Clone(live)
	storedByKey := make(map[string]db.WorkspaceRuntimeSession, len(stored))
	for _, session := range stored {
		if session.SessionKey != "" {
			storedByKey[session.SessionKey] = session
		}
	}
	seen := make(map[string]struct{}, len(sessions))
	for i, session := range sessions {
		seen[session.Key] = struct{}{}
		if storedSession, ok := storedByKey[session.Key]; ok {
			sessions[i].DisplayRegion = normalizeRuntimeDisplayRegion(
				storedSession.DisplayRegion,
				session,
			)
		} else {
			sessions[i].DisplayRegion = normalizeRuntimeDisplayRegion("", session)
		}
	}
	for _, session := range stored {
		if session.SessionKey == "" {
			continue
		}
		if _, ok := seen[session.SessionKey]; ok {
			continue
		}
		sessions = append(sessions, storedRuntimeSessionInfo(session))
		seen[session.SessionKey] = struct{}{}
	}
	slices.SortFunc(sessions, localruntime.SessionInfo.Compare)
	return sessions
}

func storedRuntimeSessionInfo(
	session db.WorkspaceRuntimeSession,
) localruntime.SessionInfo {
	targetKey := strings.TrimSpace(session.TargetKey)
	kind := localruntime.LaunchTargetKind(strings.TrimSpace(session.Kind))
	if kind == "" {
		kind = localruntime.LaunchTargetAgent
	}
	if targetKey == string(localruntime.LaunchTargetPlainShell) ||
		kind == localruntime.LaunchTargetPlainShell {
		targetKey = string(localruntime.LaunchTargetPlainShell)
		kind = localruntime.LaunchTargetPlainShell
	}
	label := strings.TrimSpace(session.Label)
	if label == "" {
		if kind == localruntime.LaunchTargetPlainShell {
			label = "Shell"
		} else {
			label = targetKey
		}
	}
	return localruntime.SessionInfo{
		Key:         session.SessionKey,
		WorkspaceID: session.WorkspaceID,
		TargetKey:   targetKey,
		Label:       label,
		Kind:        kind,
		Status:      localruntime.SessionStatusError,
		DisplayRegion: normalizeRuntimeDisplayRegion(session.DisplayRegion, localruntime.SessionInfo{
			TargetKey: targetKey,
			Kind:      kind,
		}),
		CreatedAt:   session.CreatedAt,
		TmuxSession: session.TmuxSession,
	}
}

func (s *Handler) launchWorkspaceRuntimeSession(
	ctx context.Context,
	input *launchWorkspaceRuntimeSessionInput,
) (*workspaceRuntimeSessionOutput, error) {
	session, err := s.launchWorkspaceRuntimeService(
		ctx, input.ID, input.Body.TargetKey, input.Body.DisplayRegion,
	)
	if err != nil {
		return nil, err
	}
	return &workspaceRuntimeSessionOutput{Body: session}, nil
}

func (s *Handler) LaunchWorkspaceRuntimeService(
	ctx context.Context, workspaceID, targetKey string,
) (localruntime.SessionInfo, error) {
	return s.launchWorkspaceRuntimeService(ctx, workspaceID, targetKey, "")
}

func (s *Handler) PreferredWorkspaceAgentTargetService(
	ctx context.Context, since time.Time, targetKeys []string,
) (string, bool, error) {
	return s.workspaces.PreferredWorkspaceAgentTarget(ctx, since, targetKeys)
}

func (s *Handler) launchWorkspaceRuntimeService(
	ctx context.Context, workspaceID, targetKey, displayRegion string,
) (localruntime.SessionInfo, error) {
	summary, err := s.getReadyRuntimeWorkspace(ctx, workspaceID)
	if err != nil {
		return localruntime.SessionInfo{}, err
	}
	targetKey = strings.TrimSpace(targetKey)
	if targetKey == "" {
		return localruntime.SessionInfo{}, httpapi.Validation("body.target_key", "target_key is required")
	}
	if workspaceRuntimeTargetIsAgent(s.runtime, targetKey) {
		if err := s.workspaces.PrepareAgentLaunchContext(
			ctx,
			workspace.PrepareAgentLaunchContextOptions{
				WorkspaceID: summary.ID, TargetKey: targetKey,
			},
		); err != nil {
			if errors.Is(err, workspace.ErrLaunchSpecRefreshRequired) ||
				errors.Is(err, workspace.ErrLaunchSpecSourceHidden) {
				return localruntime.SessionInfo{}, workspaceLaunchSpecProblem(err)
			}
			return localruntime.SessionInfo{}, httpapi.Internal("prepare agent context: " + err.Error())
		}
	}
	session, err := s.runtime.Launch(ctx, summary.ID, summary.WorktreePath, targetKey)
	if err != nil {
		return localruntime.SessionInfo{}, workspaceRuntimeLaunchError(err)
	}
	session.DisplayRegion = normalizeRuntimeDisplayRegion(displayRegion, session)
	if err := s.recordRuntimeSession(ctx, summary.ID, session, "session"); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if cleanupErr := s.runtime.RollbackLaunch(cleanupCtx, session); cleanupErr != nil {
			slog.Warn(
				"roll back unrecorded runtime session",
				"workspace_id", summary.ID, "session_key", session.Key,
				"tmux_session", session.TmuxSession, "err", cleanupErr,
			)
		}
		return localruntime.SessionInfo{}, err
	}
	s.invalidateWorkspaceEnrichment(summary.ID)
	s.forgetRecordedRuntimeSessionIfExited(ctx, session)
	return session, nil
}

// LaunchWorkspaceRuntimeSession launches a configured workspace target.
func (s *Handler) LaunchWorkspaceRuntimeSession(
	ctx context.Context, input *LaunchWorkspaceRuntimeSessionInput,
) (*workspaceRuntimeSessionOutput, error) {
	return s.launchWorkspaceRuntimeSession(ctx, input)
}

func workspaceRuntimeTargetIsAgent(runtime *localruntime.Manager, targetKey string) bool {
	if runtime == nil {
		return false
	}
	for _, target := range runtime.LaunchTargets() {
		if target.Key == targetKey {
			return target.Kind == localruntime.LaunchTargetAgent && target.Available
		}
	}
	return false
}

func (s *Handler) forgetRecordedRuntimeSessionIfExited(
	ctx context.Context,
	session localruntime.SessionInfo,
) {
	if s.workspaces == nil || s.runtime == nil || session.Key == "" {
		return
	}
	for _, live := range s.runtime.ListSessions(session.WorkspaceID) {
		if live.Key == session.Key {
			return
		}
	}
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), runtimeSessionCleanupTimeout,
	)
	defer cancel()
	if _, err := s.workspaces.ForgetRuntimeSessionAfterExit(
		cleanupCtx,
		session.WorkspaceID,
		session.Key,
		session.CreatedAt,
		session.TmuxSession,
	); err != nil {
		slog.Warn(
			"forget runtime session that exited before record completed",
			"workspace_id", session.WorkspaceID,
			"session_key", session.Key,
			"err", err,
		)
		return
	}
	s.invalidateWorkspaceEnrichment(session.WorkspaceID)
}

func (s *Handler) recordRuntimeSession(
	ctx context.Context,
	workspaceID string,
	session localruntime.SessionInfo,
	scope string,
) error {
	if scope == "" {
		scope = "session"
	}
	if err := s.workspaces.RecordRuntimeSession(
		ctx,
		db.WorkspaceRuntimeSession{
			WorkspaceID:   workspaceID,
			SessionKey:    session.Key,
			TargetKey:     session.TargetKey,
			Label:         session.Label,
			Kind:          string(session.Kind),
			DisplayRegion: session.DisplayRegion,
			Scope:         scope,
			TmuxSession:   session.TmuxSession,
			CreatedAt:     session.CreatedAt,
		},
	); err != nil {
		return httpapi.Internal("record runtime session: " + err.Error())
	}
	return nil
}

func normalizeRuntimeDisplayRegion(
	value string,
	session localruntime.SessionInfo,
) string {
	switch strings.TrimSpace(value) {
	case "workflow":
		return "workflow"
	case "terminal":
		return "terminal"
	}
	if session.TargetKey == string(localruntime.LaunchTargetPlainShell) ||
		session.Kind == localruntime.LaunchTargetPlainShell {
		return "terminal"
	}
	return "workflow"
}

func (s *Handler) stopWorkspaceRuntimeSession(
	ctx context.Context,
	input *stopWorkspaceRuntimeSessionInput,
) (*struct{}, error) {
	summary, err := s.getRuntimeWorkspace(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	if err := s.runtime.Stop(
		ctx, summary.ID, input.SessionKey,
	); err != nil {
		if errors.Is(err, localruntime.ErrSessionNotFound) {
			stopped, stopErr := s.workspaces.StopStoredRuntimeSessionByKey(
				ctx, summary.ID, input.SessionKey,
			)
			if stopErr != nil {
				return nil, httpapi.Internal(
					"stop stored runtime session: " + stopErr.Error(),
				)
			}
			if stopped {
				s.removeAgentActivityRuntimeSession(input.SessionKey)
				s.invalidateWorkspaceEnrichment(summary.ID)
				return nil, nil
			}
			return nil, httpapi.NotFound(httpapi.CodeNotFound, err.Error(), nil)
		}
		return nil, httpapi.Internal("stop runtime session: " + err.Error())
	}
	if err := s.workspaces.ForgetRuntimeSession(
		ctx, summary.ID, input.SessionKey,
	); err != nil {
		return nil, httpapi.Internal("forget runtime session: " + err.Error())
	}
	s.removeAgentActivityRuntimeSession(input.SessionKey)
	s.invalidateWorkspaceEnrichment(summary.ID)
	return nil, nil
}

func (s *Handler) getWorkspaceRuntimeSessionAttachSpec(
	ctx context.Context,
	input *getWorkspaceRuntimeSessionAttachSpecInput,
) (*runtimeAttachSpecOutput, error) {
	summary, err := s.getRuntimeWorkspace(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	if runtimeSessionIsNonTmux(
		s.runtime.ListSessions(summary.ID),
		input.SessionKey,
	) {
		return nil, httpapi.BadRequest(
			httpapi.CodeBadRequest, "runtime session is not tmux-backed", nil,
		)
	}
	stored, err := s.db.ListWorkspaceRuntimeTmuxSessions(ctx, summary.ID)
	if err != nil {
		return nil, httpapi.Internal("list runtime tmux sessions: " + err.Error())
	}
	targetKey, tmuxSession, ok := workspaceRuntimeAttachTarget(
		input.SessionKey, stored,
	)
	if !ok {
		return nil, httpapi.NotFound(httpapi.CodeNotFound, "runtime session not found", nil)
	}
	spec, err := runtimeAttachSpec(
		ctx, s.tmuxCommand(), input.SessionKey, targetKey, tmuxSession,
	)
	if err != nil {
		return nil, err
	}
	return &runtimeAttachSpecOutput{Body: spec}, nil
}

func runtimeSessionIsNonTmux(
	sessions []localruntime.SessionInfo,
	sessionKey string,
) bool {
	for _, session := range sessions {
		if session.Key == sessionKey {
			return session.TmuxSession == ""
		}
	}
	return false
}

func workspaceRuntimeAttachTarget(
	sessionKey string,
	stored []db.WorkspaceRuntimeSession,
) (targetKey string, tmuxSession string, ok bool) {
	for _, row := range stored {
		if row.SessionKey != sessionKey {
			continue
		}
		return row.TargetKey, row.TmuxSession, true
	}
	return "", "", false
}

func runtimeSessionTmuxSession(
	sessions []localruntime.SessionInfo,
	key string,
) string {
	for _, session := range sessions {
		if session.Key == key {
			return session.TmuxSession
		}
	}
	return ""
}

func (s *Handler) renameWorkspaceRuntimeSession(
	ctx context.Context,
	input *renameWorkspaceRuntimeSessionInput,
) (*workspaceRuntimeSessionOutput, error) {
	summary, err := s.getRuntimeWorkspace(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	label := strings.TrimSpace(input.Body.Label)
	if label == "" {
		return nil, httpapi.Validation("body.label", "label is required")
	}

	session, err := s.runtime.RenameSession(
		summary.ID, input.SessionKey, label,
	)
	if err != nil {
		if errors.Is(err, localruntime.ErrSessionNotFound) {
			stored, renamed, renameErr := s.renameStoredRuntimeSession(
				ctx, summary.ID, input.SessionKey, label,
			)
			if renameErr != nil {
				return nil, httpapi.Internal(
					"rename stored runtime session: " + renameErr.Error(),
				)
			}
			if renamed {
				return &workspaceRuntimeSessionOutput{Body: stored}, nil
			}
			return nil, httpapi.NotFound(httpapi.CodeNotFound, err.Error(), nil)
		}
		return nil, httpapi.Internal("rename runtime session: " + err.Error())
	}

	updated, err := s.workspaces.UpdateRuntimeSessionLabel(
		ctx, summary.ID, input.SessionKey, session.Label,
	)
	if err != nil {
		return nil, httpapi.Internal("update runtime session label: " + err.Error())
	}
	if !updated {
		if err := s.recordRuntimeSession(ctx, summary.ID, session, "session"); err != nil {
			return nil, err
		}
	}
	return &workspaceRuntimeSessionOutput{Body: session}, nil
}

func (s *Handler) renameStoredRuntimeSession(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	label string,
) (localruntime.SessionInfo, bool, error) {
	updated, err := s.workspaces.UpdateRuntimeSessionLabel(
		ctx, workspaceID, sessionKey, label,
	)
	if err != nil || !updated {
		return localruntime.SessionInfo{}, false, err
	}
	stored, err := s.workspaces.RuntimeSessionsForWorkspace(ctx, workspaceID)
	if err != nil {
		return localruntime.SessionInfo{}, false, err
	}
	for _, session := range stored {
		if session.SessionKey == sessionKey {
			return storedRuntimeSessionInfo(session), true, nil
		}
	}
	return localruntime.SessionInfo{}, false, nil
}

func (s *Handler) getRuntimeWorkspace(
	ctx context.Context,
	id string,
) (*db.WorkspaceSummary, error) {
	if s.workspaces == nil || s.runtime == nil {
		return nil, httpapi.ServiceUnavailable("workspace runtime not configured")
	}

	summary, err := s.workspaces.GetSummary(ctx, id)
	if err != nil {
		return nil, httpapi.Internal("get workspace failed")
	}
	if summary == nil {
		return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, "workspace not found", nil)
	}
	return summary, nil
}

func (s *Handler) getReadyRuntimeWorkspace(
	ctx context.Context,
	id string,
) (*db.WorkspaceSummary, error) {
	summary, err := s.getRuntimeWorkspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if summary.Status != "ready" {
		return nil, httpapi.Conflict(httpapi.CodeConflict,
			"workspace not ready (status: "+summary.Status+")", nil)
	}
	return summary, nil
}

func workspaceRuntimeLaunchError(err error) error {
	msg := err.Error()
	if errors.Is(err, tokenauth.ErrMissingToken) {
		return httpapi.BadRequest(httpapi.CodeBadRequest, msg, nil)
	}
	if strings.Contains(msg, "target not found") {
		return httpapi.NotFound(httpapi.CodeNotFound, msg, nil)
	}
	if strings.Contains(msg, "not available") ||
		strings.Contains(msg, "no command") {
		return httpapi.BadRequest(httpapi.CodeBadRequest, msg, nil)
	}
	return httpapi.Internal("launch session: " + msg)
}

// DeleteWorkspace tears down a kenn-forge-managed workspace.
func (s *Handler) DeleteWorkspace(
	ctx context.Context, input *DeleteWorkspaceInput,
) (*struct{}, error) {
	if s.workspaces == nil || s.db == nil {
		return nil, httpapi.ServiceUnavailable("workspace manager not configured")
	}

	setupDone := s.markWorkspaceDeleting(input.ID)
	deleted := false
	// A successful deletion keeps admission closed permanently so queued setup
	// dispatchers cannot recreate resources for the removed row; a failed one
	// reopens admission only after every concurrent deletion has finished.
	defer func() { s.finishWorkspaceDeleting(input.ID, deleted) }()

	if err := waitForWorkspaceSetup(ctx, setupDone); err != nil {
		return nil, httpapi.Internal("wait for workspace setup: " + err.Error())
	}
	ws, err := s.db.GetWorkspace(ctx, input.ID)
	if err != nil {
		return nil, httpapi.Internal("get workspace: " + err.Error())
	}
	if ws == nil {
		// DELETE is idempotent. A stale projection or a lost response can leave
		// the caller retrying after the workspace has already been removed.
		deleted = true
		return nil, nil
	}
	if problem := workspaceDeletionLifecycleProblem(ws.Status); problem != nil {
		return nil, problem
	}
	admitted := false
	err = s.runWorkspaceDeletion(ctx, input.ID, nil, input.Force, func(admitCtx context.Context) error {
		started, err := s.db.BeginWorkspaceDeletion(admitCtx, input.ID)
		if err != nil {
			return err
		}
		if !started {
			return errWorkspaceDeletionInProgress
		}
		admitted = true
		s.broadcastWorkspaceStatus(input.ID)
		return nil
	})
	if err != nil {
		if admitted {
			s.persistWorkspaceDeletionFailure(input.ID, err.Error())
		}
		if _, ok := errors.AsType[*workspaceDirtyDeletionError](err); ok {
			current, getErr := s.db.GetWorkspace(ctx, input.ID)
			if getErr != nil {
				return nil, httpapi.Internal("recheck workspace before dirty response: " + getErr.Error())
			}
			if current == nil {
				return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, "workspace not found", nil)
			}
			if problem := workspaceDeletionLifecycleProblem(current.Status); problem != nil {
				return nil, problem
			}
			return nil, httpapi.Conflict(httpapi.CodeWorktreeDirty, err.Error(), nil)
		}
		if errors.Is(err, errWorkspaceDeletionInProgress) {
			return nil, httpapi.Conflict(httpapi.CodeWorkspaceDeletionInProgress, err.Error(), nil)
		}
		if errors.Is(err, db.ErrWorkspaceSetupInProgress) {
			return nil, httpapi.Conflict(httpapi.CodeWorkspaceSetupInProgress, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceNotFound) {
			return nil, httpapi.NotFound(httpapi.CodeWorkspaceNotFound, err.Error(), nil)
		}
		if errors.Is(err, workspace.ErrWorkspaceOwnershipUnproven) {
			return nil, httpapi.Conflict(httpapi.CodeConflict, err.Error(), nil)
		}
		return nil, httpapi.Internal("delete workspace: " + err.Error())
	}

	deleted = true
	s.publishWorkspaceDeleted(ws)
	s.clearInitialMessageAttempts(input.ID)
	return nil, nil
}

func workspaceDeletionLifecycleProblem(status string) error {
	switch status {
	case "creating":
		return httpapi.Conflict(
			httpapi.CodeWorkspaceSetupInProgress,
			"workspace setup is still in progress",
			nil,
		)
	case "deleting":
		return httpapi.Conflict(
			httpapi.CodeWorkspaceDeletionInProgress,
			errWorkspaceDeletionInProgress.Error(),
			nil,
		)
	default:
		return nil
	}
}
