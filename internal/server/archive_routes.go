package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/forge/internal/archive"
	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

type archiveRepositoryRef struct {
	Provider     string `json:"provider"`
	PlatformHost string `json:"platform_host"`
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	RepoPath     string `json:"repo_path"`
}

type archiveMutationBody struct {
	All          bool                   `json:"all"`
	Repositories []archiveRepositoryRef `json:"repositories,omitempty"`
}

type archiveMutationInput struct{ Body archiveMutationBody }

type archiveStatusInput struct {
	Repositories []string `query:"repo,explode" doc:"Repeated provider|platform_host/repo_path filters."`
}

type archiveReportInput struct {
	Start        string   `query:"start" required:"true" doc:"Inclusive UTC RFC3339 boundary."`
	End          string   `query:"end" required:"true" doc:"Exclusive UTC RFC3339 boundary."`
	Repositories []string `query:"repo,explode" doc:"Repeated provider|platform_host/repo_path filters."`
	Verbose      bool     `query:"verbose"`
}

type archiveCoverageResponse struct {
	Issues         db.ArchiveCoverage `json:"issues" enum:"unknown,supported,unsupported"`
	MergeRequests  db.ArchiveCoverage `json:"merge_requests" enum:"unknown,supported,unsupported"`
	Comments       db.ArchiveCoverage `json:"comments" enum:"unknown,supported,unsupported"`
	Reviews        db.ArchiveCoverage `json:"reviews" enum:"unknown,supported,unsupported"`
	InlineComments db.ArchiveCoverage `json:"inline_comments" enum:"unknown,supported,unsupported"`
}

type archiveProgressCountsResponse struct {
	Items             int `json:"items"`
	CompleteItems     int `json:"complete_items"`
	PendingItems      int `json:"pending_items"`
	FailedItems       int `json:"failed_items"`
	UnsupportedItems  int `json:"unsupported_items"`
	InaccessibleItems int `json:"inaccessible_items"`
}

type archiveFailureResponse struct {
	Code        string     `json:"code"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
}

type archiveStatusResponse struct {
	Repository             archiveRepositoryRef          `json:"repository"`
	CollectionMode         db.ArchiveCollectionMode      `json:"collection_mode" enum:"discovery,full"`
	OperatorState          db.ArchiveOperatorState       `json:"operator_state" enum:"active,paused"`
	Status                 db.ArchiveStatus              `json:"status" enum:"running,waiting_for_budget,current,partial,paused,blocked"`
	ActivePhases           []db.ArchivePhase             `json:"active_phases" enum:"issue_inventory,merge_request_inventory,hydration,prompt_maintenance"`
	Counts                 archiveProgressCountsResponse `json:"counts"`
	Coverage               archiveCoverageResponse       `json:"coverage"`
	BudgetWaitUntil        *time.Time                    `json:"budget_wait_until,omitempty"`
	InitialStartedAt       *time.Time                    `json:"initial_started_at,omitempty"`
	InitialCompletedAt     *time.Time                    `json:"initial_completed_at,omitempty"`
	MaintenanceWatermark   *time.Time                    `json:"maintenance_watermark,omitempty"`
	MaintenanceSucceededAt *time.Time                    `json:"maintenance_succeeded_at,omitempty"`
	Failure                *archiveFailureResponse       `json:"failure,omitempty"`
}

type archiveStatusesOutput = httpapi.BodyOutput[[]archiveStatusResponse]

type archiveReportCountsResponse struct {
	IssuesOpened         int `json:"issues_opened"`
	IssuesClosed         int `json:"issues_closed"`
	MergeRequestsOpened  int `json:"merge_requests_opened"`
	MergeRequestsMerged  int `json:"merge_requests_merged"`
	OrdinaryComments     int `json:"ordinary_comments"`
	ReviewsSubmitted     int `json:"reviews_submitted"`
	InlineReviewComments int `json:"inline_review_comments"`
}

type archiveReportCoverageResponse struct {
	Status                 string     `json:"status" enum:"running,waiting_for_budget,current,partial,paused,blocked"`
	ActivePhases           []string   `json:"active_phases"`
	CollectionMode         string     `json:"collection_mode" enum:"discovery,full"`
	OperatorState          string     `json:"operator_state" enum:"active,paused"`
	Issues                 string     `json:"issues" enum:"unknown,supported,unsupported"`
	MergeRequests          string     `json:"merge_requests" enum:"unknown,supported,unsupported"`
	Comments               string     `json:"comments" enum:"unknown,supported,unsupported"`
	Reviews                string     `json:"reviews" enum:"unknown,supported,unsupported"`
	InlineComments         string     `json:"inline_comments" enum:"unknown,supported,unsupported"`
	InitialCompletedAt     *time.Time `json:"initial_completed_at,omitempty"`
	MaintenanceSucceededAt *time.Time `json:"maintenance_succeeded_at,omitempty"`
	BudgetWaitUntil        *time.Time `json:"budget_wait_until,omitempty"`
	ArchivedItems          int        `json:"archived_items"`
	UnsupportedItems       int        `json:"unsupported_items"`
	InaccessibleItems      int        `json:"inaccessible_items"`
}

type archiveReportRepositoryResponse struct {
	Repository archiveRepositoryRef          `json:"repository"`
	Coverage   archiveReportCoverageResponse `json:"coverage"`
	Counts     archiveReportCountsResponse   `json:"counts"`
}

type archiveReportContributorResponse struct {
	Provider     string                      `json:"provider"`
	PlatformHost string                      `json:"platform_host"`
	Login        string                      `json:"login"`
	Counts       archiveReportCountsResponse `json:"counts"`
}

type archiveReportActivityResponse struct {
	Repository         archiveRepositoryRef `json:"repository"`
	Kind               string               `json:"kind" enum:"issue,issue_closed,merge_request,merge_request_merged,ordinary_comment,review,inline_review_comment"`
	ItemNumber         int                  `json:"item_number"`
	ProviderExternalID string               `json:"provider_external_id"`
	Title              string               `json:"title"`
	Author             string               `json:"author"`
	Actor              string               `json:"actor,omitempty"`
	OccurredAt         time.Time            `json:"occurred_at"`
	Body               string               `json:"body"`
	URL                string               `json:"url"`
	Comments           int                  `json:"comments,omitempty"`
	Additions          int                  `json:"additions,omitempty"`
	Deletions          int                  `json:"deletions,omitempty"`
	FilesChanged       *int                 `json:"files_changed,omitempty"`
	MergeCommitSHA     string               `json:"merge_commit_sha,omitempty"`
}

type archiveReportResponse struct {
	ReportSchema string                             `json:"schema"`
	Start        time.Time                          `json:"start"`
	End          time.Time                          `json:"end"`
	Repositories []archiveReportRepositoryResponse  `json:"repositories"`
	Totals       archiveReportCountsResponse        `json:"totals"`
	Contributors []archiveReportContributorResponse `json:"contributors"`
	Activity     []archiveReportActivityResponse    `json:"activity,omitempty"`
}

func (*archiveReportResponse) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	if schema == nil {
		return nil
	}
	property := schema.Properties["schema"]
	if property == nil {
		return schema
	}
	if property.Extensions == nil {
		property.Extensions = map[string]any{}
	}
	property.Extensions["x-go-name"] = "ReportSchema"
	return schema
}

type archiveReportOutput = httpapi.BodyOutput[archiveReportResponse]

func (s *Server) registerArchiveAPI(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "start-archives", Method: http.MethodPost, Path: "/archive/start",
		DefaultStatus: http.StatusOK, Summary: "Start historical archives", Tags: []string{"Archive"},
	}, s.startArchives)
	huma.Register(api, huma.Operation{
		OperationID: "pause-archives", Method: http.MethodPost, Path: "/archive/pause",
		DefaultStatus: http.StatusOK, Summary: "Pause historical archives", Tags: []string{"Archive"},
	}, s.pauseArchives)
	huma.Register(api, huma.Operation{
		OperationID: "list-archive-status", Method: http.MethodGet, Path: "/archive/status",
		DefaultStatus: http.StatusOK, Summary: "List historical archive status", Tags: []string{"Archive"},
	}, s.listArchiveStatus)
	huma.Register(api, huma.Operation{
		OperationID: "get-archive-report", Method: http.MethodGet, Path: "/archive/report",
		DefaultStatus: http.StatusOK, Summary: "Get historical archive activity report", Tags: []string{"Archive"},
	}, s.getArchiveReport)
	huma.Register(api, huma.Operation{
		OperationID: "list-archive-pacing", Method: http.MethodGet, Path: "/archive/pacing",
		DefaultStatus: http.StatusOK, Summary: "List archive hydration pacing per provider credential", Tags: []string{"Archive"},
	}, s.listArchivePacing)
}

// archivePacingStatusResponse reports one provider credential's archive
// hydration headroom. Limit, remaining, reserve, and reset all describe the
// binding pool — the resource with the least headroom, the constraint that
// currently limits admission — so the numbers are mutually consistent.
// Available is that pool's remaining above its own limit/5 reserve. Known is
// false when the combined window is unavailable (a required pool missing,
// expired, or never observed) — the state archive admission defers as
// "provider quota unknown" — and the pacing numbers are zero.
type archivePacingStatusResponse struct {
	Provider     string `json:"provider"`
	PlatformHost string `json:"platform_host"`
	Principal    string `json:"principal"`
	Source       string `json:"source" enum:"provider"`
	Known        bool   `json:"known"`
	Limit        int    `json:"limit"`
	Remaining    int    `json:"remaining"`
	Reserve      int    `json:"reserve"`
	Available    int    `json:"available"`
	ResetAt      string `json:"reset_at,omitempty" format:"date-time"`
}

type archivePacingOutput = httpapi.BodyOutput[[]archivePacingStatusResponse]

func (s *Server) listArchivePacing(
	_ context.Context, _ *struct{},
) (*archivePacingOutput, error) {
	statuses := []archivePacingStatusResponse{}
	if s.syncer == nil {
		return &archivePacingOutput{Body: statuses}, nil
	}
	registry := s.syncer.QuotaRegistry()
	resources := []ghclient.QuotaResource{
		ghclient.QuotaResourceREST, ghclient.QuotaResourceGraphQL,
	}
	seen := map[ghclient.IdentityKey]bool{}
	for _, pool := range registry.Snapshot() {
		if seen[pool.Identity] {
			continue
		}
		seen[pool.Identity] = true
		status := archivePacingStatusResponse{
			Provider:     string(platform.KindGitHub),
			PlatformHost: pool.Identity.Host,
			Principal:    pool.Identity.Principal,
			Source:       "provider",
		}
		if window, ok := registry.PacingWindow(pool.Identity, resources); ok {
			_, binding := window.ArchiveBindingResource()
			status.Known = true
			status.Limit = binding.Limit
			status.Remaining = binding.Remaining
			status.Reserve = binding.Remaining - binding.Headroom
			status.Available = max(binding.Headroom, 0)
			status.ResetAt = formatUTCRFC3339(binding.ResetAt)
		}
		statuses = append(statuses, status)
	}
	return &archivePacingOutput{Body: statuses}, nil
}

func (s *Server) startArchives(
	ctx context.Context,
	input *archiveMutationInput,
) (*archiveStatusesOutput, error) {
	if err := s.requireSync(); err != nil {
		return nil, err
	}
	if s.archive == nil {
		return nil, httpapi.ServiceUnavailable("archive service not configured")
	}
	refs, all, err := archiveMutationRefs(input.Body)
	if err != nil {
		return nil, err
	}
	var statuses []archive.Status
	if all {
		statuses, err = s.archive.StartAll(ctx)
	} else {
		statuses, err = s.archive.Start(ctx, refs)
	}
	if err != nil {
		return nil, archiveOperationProblem(err)
	}
	return &archiveStatusesOutput{Body: archiveStatusResponses(statuses)}, nil
}

func (s *Server) pauseArchives(
	ctx context.Context,
	input *archiveMutationInput,
) (*archiveStatusesOutput, error) {
	if s.archive == nil {
		return nil, httpapi.ServiceUnavailable("archive service not configured")
	}
	refs, all, err := archiveMutationRefs(input.Body)
	if err != nil {
		return nil, err
	}
	var statuses []archive.Status
	if all {
		statuses, err = s.archive.PauseAll(ctx)
	} else {
		statuses, err = s.archive.Pause(ctx, refs)
	}
	if err != nil {
		return nil, archiveOperationProblem(err)
	}
	return &archiveStatusesOutput{Body: archiveStatusResponses(statuses)}, nil
}

func (s *Server) listArchiveStatus(
	ctx context.Context,
	input *archiveStatusInput,
) (*archiveStatusesOutput, error) {
	if s.archive == nil {
		return nil, httpapi.ServiceUnavailable("archive service not configured")
	}
	refs, err := archiveQueryRefs(input.Repositories)
	if err != nil {
		return nil, err
	}
	statuses, err := s.archive.Status(ctx, refs)
	if err != nil {
		return nil, archiveOperationProblem(err)
	}
	return &archiveStatusesOutput{Body: archiveStatusResponses(statuses)}, nil
}

func (s *Server) getArchiveReport(
	ctx context.Context,
	input *archiveReportInput,
) (*archiveReportOutput, error) {
	if s.archive == nil {
		return nil, httpapi.ServiceUnavailable("archive service not configured")
	}
	start, err := parseArchiveUTCTime("query.start", input.Start)
	if err != nil {
		return nil, err
	}
	end, err := parseArchiveUTCTime("query.end", input.End)
	if err != nil {
		return nil, err
	}
	if !start.Before(end) {
		return nil, httpapi.Validation("query.end", "end must be after start")
	}
	refs, err := archiveQueryRefs(input.Repositories)
	if err != nil {
		return nil, err
	}
	model, err := s.archive.Report(ctx, archive.ReportOptions{
		Start: start, End: end, Repositories: refs, Detailed: input.Verbose,
	})
	if err != nil {
		return nil, archiveOperationProblem(err)
	}
	return &archiveReportOutput{Body: archiveReportModelResponse(model)}, nil
}

func archiveMutationRefs(body archiveMutationBody) ([]platform.RepoRef, bool, error) {
	if body.All && len(body.Repositories) > 0 {
		return nil, false, httpapi.Validation("body.repositories", "all and repositories are mutually exclusive")
	}
	if !body.All && len(body.Repositories) == 0 {
		return nil, false, httpapi.Validation("body.repositories", "repositories must not be empty when all is false")
	}
	refs := make([]platform.RepoRef, len(body.Repositories))
	for i, ref := range body.Repositories {
		converted := platform.RepoRef{
			Platform: platform.Kind(ref.Provider), Host: ref.PlatformHost,
			Owner: ref.Owner, Name: ref.Name, RepoPath: ref.RepoPath,
		}
		if err := platform.ValidateCanonicalRepoRef(converted); err != nil {
			return nil, false, httpapi.Validation(
				"body.repositories",
				"repository references must carry canonical provider, host, owner, name, and repo_path",
			)
		}
		refs[i] = converted
	}
	return refs, body.All, nil
}

func archiveQueryRefs(values []string) ([]platform.RepoRef, error) {
	values = splitRepoFilterValues(values)
	refs := make([]platform.RepoRef, 0, len(values))
	for _, value := range values {
		provider, host, _, _, repoPath := parseRepoFilter(value)
		separator := strings.LastIndex(repoPath, "/")
		if provider == "" || host == "" || separator <= 0 || separator == len(repoPath)-1 {
			return nil, httpapi.Validation(
				"query.repo", "repo filter must be provider|platform_host/repo_path",
			)
		}
		ref := platform.RepoRef{
			Platform: platform.Kind(provider), Host: host, Owner: repoPath[:separator],
			Name: repoPath[separator+1:], RepoPath: repoPath,
		}
		if err := platform.ValidateCanonicalRepoRef(ref); err != nil {
			return nil, httpapi.Validation(
				"query.repo", "repo filter must be a canonical provider|platform_host/repo_path",
			)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func parseArchiveUTCTime(field, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, httpapi.Validation(field, "timestamp must be RFC3339")
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return time.Time{}, httpapi.Validation(field, "timestamp must use UTC")
	}
	return parsed.UTC(), nil
}

func archiveStatusResponses(statuses []archive.Status) []archiveStatusResponse {
	responses := make([]archiveStatusResponse, len(statuses))
	for i, status := range statuses {
		counts := status.Progress.Counts
		responses[i] = archiveStatusResponse{
			Repository: archiveRepositoryRef{
				Provider: string(status.Repo.Platform), PlatformHost: status.Repo.Host,
				Owner: status.Repo.Owner, Name: status.Repo.Name, RepoPath: status.Repo.RepoPath,
			},
			CollectionMode: status.State.CollectionMode, OperatorState: status.State.OperatorState,
			Status: status.Progress.Status, ActivePhases: status.Progress.ActivePhases,
			Counts: archiveProgressCountsResponse{
				Items: counts.ItemCount, CompleteItems: counts.CompleteItemCount,
				PendingItems: counts.PendingItemCount, FailedItems: counts.FailedItemCount,
				UnsupportedItems:  counts.UnsupportedItemCount,
				InaccessibleItems: counts.InaccessibleItemCount,
			},
			Coverage: archiveCoverageResponse{
				Issues:        status.State.IssuesCoverage,
				MergeRequests: status.State.MergeRequestsCoverage,
				Comments:      status.State.CommentsCoverage, Reviews: status.State.ReviewsCoverage,
				InlineComments: status.State.InlineCommentsCoverage,
			},
			BudgetWaitUntil:        status.Progress.BudgetWaitUntil,
			InitialStartedAt:       status.State.InitialStartedAt,
			InitialCompletedAt:     status.State.InitialCompletedAt,
			MaintenanceWatermark:   status.State.MaintenanceWatermark,
			MaintenanceSucceededAt: status.State.MaintenanceSucceededAt,
		}
		if status.State.LastErrorCode != nil {
			responses[i].Failure = &archiveFailureResponse{
				Code: *status.State.LastErrorCode, NextRetryAt: status.State.NextRetryAt,
			}
		}
	}
	return responses
}

func archiveOperationProblem(err error) error {
	if limit, ok := errors.AsType[*report.LimitError](err); ok {
		return httpapi.NewProblem(
			http.StatusRequestEntityTooLarge, httpapi.CodePayloadTooLarge,
			"archive report exceeds detailed limits", map[string]any{
				"reason": "reportTooLarge", "observedRecords": limit.ObservedRecords,
				"maxRecords": limit.MaxRecords, "observedTextBytes": limit.ObservedTextBytes,
				"maxTextBytes": limit.MaxTextBytes,
			},
		)
	}
	if errors.Is(err, archive.ErrEmptyReportScope) {
		return httpapi.BadRequest(httpapi.CodeBadRequest, err.Error(), nil)
	}
	if _, ok := errors.AsType[*platform.Error](err); ok {
		return httpapi.MapPlatformError(err)
	}
	if _, ok := errors.AsType[*db.ArchiveRepoStateNotFoundError](err); ok {
		return httpapi.BadRequest(httpapi.CodeBadRequest, "repository is not available for archiving", nil)
	}
	return httpapi.Internal("archive operation failed")
}

func archiveReportModelResponse(model report.Model) archiveReportResponse {
	response := archiveReportResponse{
		ReportSchema: model.Schema, Start: model.Start, End: model.End,
		Repositories: make([]archiveReportRepositoryResponse, len(model.Repositories)),
		Totals:       archiveReportCounts(model.Totals),
		Contributors: make([]archiveReportContributorResponse, len(model.Contributors)),
	}
	for i, repository := range model.Repositories {
		coverage := repository.Coverage
		response.Repositories[i] = archiveReportRepositoryResponse{
			Repository: archiveReportRepository(repository.Repository),
			Coverage: archiveReportCoverageResponse{
				Status: coverage.Status, ActivePhases: coverage.ActivePhases,
				CollectionMode: coverage.CollectionMode, OperatorState: coverage.OperatorState,
				Issues: coverage.Issues, MergeRequests: coverage.MergeRequests,
				Comments: coverage.Comments, Reviews: coverage.Reviews,
				InlineComments:         coverage.InlineComments,
				InitialCompletedAt:     coverage.InitialCompletedAt,
				MaintenanceSucceededAt: coverage.MaintenanceSucceededAt,
				BudgetWaitUntil:        coverage.BudgetWaitUntil, ArchivedItems: coverage.ArchivedItems,
				UnsupportedItems:  coverage.UnsupportedItems,
				InaccessibleItems: coverage.InaccessibleItems,
			},
			Counts: archiveReportCounts(repository.Counts),
		}
	}
	for i, contributor := range model.Contributors {
		response.Contributors[i] = archiveReportContributorResponse{
			Provider: contributor.Provider, PlatformHost: contributor.PlatformHost,
			Login: contributor.Login, Counts: archiveReportCounts(contributor.Counts),
		}
	}
	if model.Activity != nil {
		response.Activity = make([]archiveReportActivityResponse, len(model.Activity))
		for i, activity := range model.Activity {
			response.Activity[i] = archiveReportActivityResponse{
				Repository: archiveReportRepository(activity.Repository), Kind: string(activity.Kind),
				ItemNumber: activity.ItemNumber, ProviderExternalID: activity.ProviderExternalID,
				Title: activity.Title, Author: activity.Author, OccurredAt: activity.OccurredAt,
				Actor: activity.Actor, Body: activity.Body, URL: activity.URL,
				Comments: activity.Comments, Additions: activity.Additions,
				Deletions: activity.Deletions, FilesChanged: activity.FilesChanged,
				MergeCommitSHA: activity.MergeCommitSHA,
			}
		}
	}
	return response
}

func archiveReportRepository(ref report.RepositoryRef) archiveRepositoryRef {
	return archiveRepositoryRef{
		Provider: ref.Provider, PlatformHost: ref.PlatformHost, Owner: ref.Owner,
		Name: ref.Name, RepoPath: ref.RepoPath,
	}
}

func archiveReportCounts(counts report.Counts) archiveReportCountsResponse {
	return archiveReportCountsResponse{
		IssuesOpened: counts.IssuesOpened, IssuesClosed: counts.IssuesClosed,
		MergeRequestsOpened: counts.MergeRequestsOpened,
		MergeRequestsMerged: counts.MergeRequestsMerged,
		OrdinaryComments:    counts.OrdinaryComments, ReviewsSubmitted: counts.ReviewsSubmitted,
		InlineReviewComments: counts.InlineReviewComments,
	}
}
