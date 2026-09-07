package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

var ErrEmptyReportScope = errors.New("archive report repository scope is empty")

type ReportOptions struct {
	Start        time.Time
	End          time.Time
	Repositories []platform.RepoRef
	Detailed     bool
}

func (s *Service) Report(ctx context.Context, opts ReportOptions) (report.Model, error) {
	return s.report(ctx, opts, nil)
}

func (s *Service) report(
	ctx context.Context,
	opts ReportOptions,
	afterCoverage func() error,
) (report.Model, error) {
	if opts.Start.IsZero() || opts.End.IsZero() || !opts.Start.Before(opts.End) {
		return report.Model{}, errors.New("archive report start must precede end")
	}
	opts.Start = opts.Start.UTC()
	opts.End = opts.End.UTC()

	var requestedIDs []int64
	if len(opts.Repositories) > 0 {
		resolved, err := s.resolveRepositories(ctx, opts.Repositories, false)
		if err != nil {
			return report.Model{}, err
		}
		requestedIDs = resolvedRepoIDs(resolved)
	}

	tx, err := s.db.ReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report.Model{}, fmt.Errorf("begin archive report snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	repositories, err := db.LoadArchiveReportRepositories(ctx, tx, requestedIDs, opts.End)
	if err != nil {
		return report.Model{}, err
	}
	if len(repositories) == 0 {
		return report.Model{}, ErrEmptyReportScope
	}
	if afterCoverage != nil {
		if err := afterCoverage(); err != nil {
			return report.Model{}, fmt.Errorf("after archive report coverage: %w", err)
		}
	}
	repoIDs := make([]int64, len(repositories))
	for i := range repositories {
		repoIDs[i] = repositories[i].RepoID
	}
	counts, err := db.LoadArchiveReportCounts(ctx, tx, repoIDs, opts.Start, opts.End)
	if err != nil {
		return report.Model{}, err
	}
	var activityRows []db.ArchiveReportActivityRow
	if opts.Detailed {
		measurement, err := db.MeasureArchiveReportActivity(ctx, tx, repoIDs, opts.Start, opts.End)
		if err != nil {
			return report.Model{}, err
		}
		if err := report.ValidateDetailedSize(measurement.Records, measurement.TextBytes); err != nil {
			return report.Model{}, err
		}
		activityRows, err = db.LoadArchiveReportActivity(ctx, tx, repoIDs, opts.Start, opts.End)
		if err != nil {
			return report.Model{}, err
		}
	}

	model := buildArchiveReport(opts, repositories, counts, activityRows)
	if err := tx.Commit(); err != nil {
		return report.Model{}, fmt.Errorf("commit archive report snapshot: %w", err)
	}
	return model, nil
}

func buildArchiveReport(
	opts ReportOptions,
	repositories []db.ArchiveReportRepositoryRow,
	counts []db.ArchiveReportCountRow,
	activityRows []db.ArchiveReportActivityRow,
) report.Model {
	model := report.Model{
		Schema: report.Schema, Start: opts.Start, End: opts.End,
		Repositories: make([]report.Repository, len(repositories)),
		Contributors: []report.Contributor{},
	}
	repoIndexes := make(map[int64]int, len(repositories))
	for i, row := range repositories {
		repoIndexes[row.RepoID] = i
		model.Repositories[i] = report.Repository{
			Repository: reportRepositoryRef(
				row.Platform, row.PlatformHost, row.Owner, row.Name, row.RepoPath,
			),
			Coverage: reportCoverage(row),
		}
	}

	contributors := make(map[string]*report.Contributor)
	for _, row := range counts {
		addReportCount(&model.Totals, row.Kind, row.Count)
		repoIndex := repoIndexes[row.RepoID]
		addReportCount(&model.Repositories[repoIndex].Counts, row.Kind, row.Count)
		key := strings.Join([]string{row.Platform, row.PlatformHost, row.Author}, "\x00")
		contributor := contributors[key]
		if contributor == nil {
			contributor = &report.Contributor{
				Provider: row.Platform, PlatformHost: row.PlatformHost, Login: row.Author,
			}
			contributors[key] = contributor
		}
		addReportCount(&contributor.Counts, row.Kind, row.Count)
	}
	for _, contributor := range contributors {
		model.Contributors = append(model.Contributors, *contributor)
	}
	sort.Slice(model.Contributors, func(i, j int) bool {
		a, b := model.Contributors[i], model.Contributors[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.PlatformHost != b.PlatformHost {
			return a.PlatformHost < b.PlatformHost
		}
		return a.Login < b.Login
	})

	if opts.Detailed {
		model.Activity = make([]report.Activity, len(activityRows))
		for i, row := range activityRows {
			model.Activity[i] = report.Activity{
				Repository: reportRepositoryRef(
					row.Platform, row.PlatformHost, row.Owner, row.Name, row.RepoPath,
				),
				Kind: report.ActivityKind(row.Kind), ItemNumber: row.ItemNumber,
				ProviderExternalID: row.ProviderExternalID, Title: row.Title,
				Author: row.Author, Actor: row.Actor, OccurredAt: row.OccurredAt.UTC(),
				Body: row.Body, URL: row.URL, Comments: row.Comments,
				Additions: row.Additions, Deletions: row.Deletions,
				FilesChanged: row.FilesChanged, MergeCommitSHA: row.MergeCommitSHA,
			}
		}
	}
	return model
}

func reportCoverage(row db.ArchiveReportRepositoryRow) report.Coverage {
	phases := make([]string, len(row.Progress.ActivePhases))
	for i := range row.Progress.ActivePhases {
		phases[i] = string(row.Progress.ActivePhases[i])
	}
	return report.Coverage{
		Status: string(row.Progress.Status), ActivePhases: phases,
		CollectionMode: string(row.State.CollectionMode), OperatorState: string(row.State.OperatorState),
		Issues: string(row.State.IssuesCoverage), MergeRequests: string(row.State.MergeRequestsCoverage),
		Comments: string(row.State.CommentsCoverage), Reviews: string(row.State.ReviewsCoverage),
		InlineComments:         string(row.State.InlineCommentsCoverage),
		InitialCompletedAt:     row.State.InitialCompletedAt,
		MaintenanceSucceededAt: row.State.MaintenanceSucceededAt,
		BudgetWaitUntil:        row.Progress.BudgetWaitUntil,
		ArchivedItems:          row.Progress.Counts.ItemCount,
		UnsupportedItems:       row.Progress.Counts.UnsupportedItemCount,
		InaccessibleItems:      row.Progress.Counts.InaccessibleItemCount,
	}
}

func reportRepositoryRef(provider, host, owner, name, repoPath string) report.RepositoryRef {
	return report.RepositoryRef{
		Provider: provider, PlatformHost: host, Owner: owner, Name: name, RepoPath: repoPath,
	}
}

func addReportCount(counts *report.Counts, kind db.ArchiveReportActivityKind, count int) {
	switch kind {
	case db.ArchiveReportActivityIssue:
		counts.IssuesOpened += count
	case db.ArchiveReportActivityIssueClosed:
		counts.IssuesClosed += count
	case db.ArchiveReportActivityMergeRequest:
		counts.MergeRequestsOpened += count
	case db.ArchiveReportActivityMergeRequestMerged:
		counts.MergeRequestsMerged += count
	case db.ArchiveReportActivityOrdinaryComment:
		counts.OrdinaryComments += count
	case db.ArchiveReportActivityReview:
		counts.ReviewsSubmitted += count
	case db.ArchiveReportActivityInlineReviewComment:
		counts.InlineReviewComments += count
	}
}
