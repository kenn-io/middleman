package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/forge/internal/apiclient"
	"go.kenn.io/forge/internal/apiclient/generated"
	"go.kenn.io/forge/internal/archive/report"
	"go.kenn.io/forge/internal/config"
)

type archiveStringList []string

func (v *archiveStringList) String() string { return strings.Join(*v, ",") }
func (v *archiveStringList) Type() string   { return "repository" }
func (v *archiveStringList) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type archiveDaemonFlags struct {
	configPath string
	timeout    time.Duration
}

func addArchiveDaemonFlags(cmd *cobra.Command, flags *archiveDaemonFlags) {
	cmd.Flags().StringVar(&flags.configPath, "config", config.DefaultConfigPath(), "path to config file")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", 60*time.Second, "request timeout")
}

func runArchiveCLIAt(args []string, stdout io.Writer, now func() time.Time) error {
	cmd := newArchiveCommand(stdout, now)
	cmd.SetOut(stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(normalizeSingleDashLongFlags(args))
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	return cmd.Execute()
}

func newArchiveCommand(stdout io.Writer, now func() time.Time) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Control and report on repository archives",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newArchiveMutationCommand("start", stdout),
		newArchiveMutationCommand("pause", stdout),
		newArchiveStatusCommand(stdout),
		newArchiveReportCommand(stdout, now),
	)
	return cmd
}

func newArchiveMutationCommand(action string, stdout io.Writer) *cobra.Command {
	flags := archiveDaemonFlags{}
	var all bool
	var repositories archiveStringList
	cmd := &cobra.Command{
		Use:   action,
		Short: action + " repository archiving",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveMutation(action, flags, all, repositories, stdout)
		},
	}
	addArchiveDaemonFlags(cmd, &flags)
	cmd.Flags().BoolVar(&all, "all", false, "operate on every configured repository")
	cmd.Flags().Var(&repositories, "repo", "provider|host/repo_path; repeat for multiple repositories")
	return cmd
}

func runArchiveMutation(command string, daemonFlags archiveDaemonFlags, all bool, repositories []string, stdout io.Writer) error {
	if all && len(repositories) > 0 {
		return errors.New("--all and --repo are mutually exclusive")
	}
	if !all && len(repositories) == 0 {
		return errors.New("one of --all or --repo is required")
	}
	refs, err := parseArchiveRepositoryRefs(repositories)
	if err != nil {
		return err
	}
	client, err := newArchiveDaemonClient(&daemonFlags)
	if err != nil {
		return err
	}
	body := generated.ArchiveMutationBody{All: all}
	if len(refs) > 0 {
		body.Repositories = &refs
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonFlags.timeout)
	defer cancel()
	var statuses []generated.ArchiveStatusResponse
	if command == "start" {
		response, requestErr := client.HTTP.StartArchivesWithResponse(ctx, body)
		if requestErr != nil {
			return fmt.Errorf("start archive request: %w", requestErr)
		}
		if response.JSON200 == nil {
			return archiveAPIProblem("start archive", response.StatusCode(), response.ApplicationproblemJSONDefault)
		}
		statuses = *response.JSON200
	} else {
		response, requestErr := client.HTTP.PauseArchivesWithResponse(ctx, body)
		if requestErr != nil {
			return fmt.Errorf("pause archive request: %w", requestErr)
		}
		if response.JSON200 == nil {
			return archiveAPIProblem("pause archive", response.StatusCode(), response.ApplicationproblemJSONDefault)
		}
		statuses = *response.JSON200
	}
	return writeArchiveJSON(stdout, statuses)
}

func newArchiveStatusCommand(stdout io.Writer) *cobra.Command {
	flags := archiveDaemonFlags{}
	var repositories archiveStringList
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show repository archive status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveStatus(flags, repositories, stdout)
		},
	}
	addArchiveDaemonFlags(cmd, &flags)
	cmd.Flags().Bool("json", false, "emit JSON (status output is always JSON)")
	cmd.Flags().Var(&repositories, "repo", "provider|host/repo_path; repeat for multiple repositories")
	return cmd
}

func runArchiveStatus(daemonFlags archiveDaemonFlags, repositories []string, stdout io.Writer) error {
	refs, err := parseArchiveRepositoryRefs(repositories)
	if err != nil {
		return err
	}
	client, err := newArchiveDaemonClient(&daemonFlags)
	if err != nil {
		return err
	}
	params := &generated.ListArchiveStatusParams{}
	if len(refs) > 0 {
		values := archiveRepositoryFilters(refs)
		params.Repo = &values
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonFlags.timeout)
	defer cancel()
	response, err := client.HTTP.ListArchiveStatusWithResponse(ctx, params)
	if err != nil {
		return fmt.Errorf("archive status request: %w", err)
	}
	if response.JSON200 == nil {
		return archiveAPIProblem("archive status", response.StatusCode(), response.ApplicationproblemJSONDefault)
	}
	return writeArchiveJSON(stdout, *response.JSON200)
}

type archiveReportOptions struct {
	daemonFlags  archiveDaemonFlags
	days         int
	startValue   string
	endValue     string
	format       string
	verbose      bool
	output       string
	repositories archiveStringList
}

func newArchiveReportCommand(stdout io.Writer, now func() time.Time) *cobra.Command {
	opts := archiveReportOptions{}
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render repository archive activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchiveReport(opts, cmd.Flags().Changed("days"), stdout, now)
		},
	}
	addArchiveDaemonFlags(cmd, &opts.daemonFlags)
	cmd.Flags().IntVar(&opts.days, "days", 0, "rolling number of 24-hour UTC periods")
	cmd.Flags().StringVar(&opts.startValue, "start", "", "inclusive UTC date or RFC3339 boundary")
	cmd.Flags().StringVar(&opts.endValue, "end", "", "inclusive UTC date or exclusive RFC3339 boundary")
	cmd.Flags().StringVar(&opts.format, "format", "markdown", "output format: markdown or json")
	cmd.Flags().BoolVar(&opts.verbose, "verbose", false, "include bounded activity details")
	cmd.Flags().StringVar(&opts.output, "output", "", "write output atomically to this file")
	cmd.Flags().Var(&opts.repositories, "repo", "provider|host/repo_path; repeat for multiple repositories")
	return cmd
}

func runArchiveReport(opts archiveReportOptions, daysSet bool, stdout io.Writer, now func() time.Time) error {
	if daysSet && opts.days <= 0 {
		return errors.New("--days must be positive")
	}
	if opts.format != "markdown" && opts.format != "json" {
		return fmt.Errorf("unsupported archive report format %q; use markdown or json", opts.format)
	}
	start, end, err := parseArchiveReportRange(now().UTC(), opts.days, opts.startValue, opts.endValue)
	if err != nil {
		return err
	}
	refs, err := parseArchiveRepositoryRefs(opts.repositories)
	if err != nil {
		return err
	}
	client, err := newArchiveDaemonClient(&opts.daemonFlags)
	if err != nil {
		return err
	}
	params := &generated.GetArchiveReportParams{
		Start: start.Format(time.RFC3339), End: end.Format(time.RFC3339),
	}
	if len(refs) > 0 {
		values := archiveRepositoryFilters(refs)
		params.Repo = &values
	}
	if opts.verbose {
		params.Verbose = &opts.verbose
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.daemonFlags.timeout)
	defer cancel()
	response, err := client.HTTP.GetArchiveReportWithResponse(ctx, params)
	if err != nil {
		return fmt.Errorf("archive report request: %w", err)
	}
	if response.JSON200 == nil {
		return archiveAPIProblem("archive report", response.StatusCode(), response.ApplicationproblemJSONDefault)
	}
	model, err := archiveReportFromAPI(*response.JSON200)
	if err != nil {
		return fmt.Errorf("decode archive report: %w", err)
	}
	rendered, err := renderArchiveReport(model, opts.format)
	if err != nil {
		return err
	}
	if opts.output != "" {
		return writeArchiveOutput(opts.output, rendered)
	}
	_, err = io.WriteString(stdout, rendered)
	return err
}

func newArchiveDaemonClient(flags *archiveDaemonFlags) (*apiclient.Client, error) {
	daemon, err := discoverDaemonHTTP(flags.configPath, flags.timeout)
	if err != nil {
		return nil, err
	}
	client, err := apiclient.NewWithHTTPClient(daemon.BaseURL, daemon.Client)
	if err != nil {
		return nil, fmt.Errorf("create archive API client: %w", err)
	}
	return client, nil
}

func parseArchiveRepositoryRefs(values []string) ([]generated.ArchiveRepositoryRef, error) {
	refs := make([]generated.ArchiveRepositoryRef, len(values))
	for i, value := range values {
		ref, err := parseArchiveRepositoryRef(value)
		if err != nil {
			return nil, fmt.Errorf("invalid --repo %q: %w", value, err)
		}
		refs[i] = ref
	}
	return refs, nil
}

func archiveRepositoryFilters(refs []generated.ArchiveRepositoryRef) []string {
	values := make([]string, len(refs))
	for i, ref := range refs {
		values[i] = ref.Provider + "|" + ref.PlatformHost + "/" + ref.RepoPath
	}
	return values
}

func parseArchiveRepositoryRef(value string) (generated.ArchiveRepositoryRef, error) {
	provider, remainder, ok := strings.Cut(strings.TrimSpace(value), "|")
	provider = strings.TrimSpace(provider)
	if !ok || provider == "" {
		return generated.ArchiveRepositoryRef{}, errors.New("expected provider|host/repo_path")
	}
	host, repoPath, ok := strings.Cut(remainder, "/")
	host = strings.TrimSpace(host)
	repoPath = strings.TrimSpace(repoPath)
	if !ok || host == "" || repoPath == "" {
		return generated.ArchiveRepositoryRef{}, errors.New("expected provider|host/repo_path")
	}
	lastSlash := strings.LastIndex(repoPath, "/")
	if lastSlash <= 0 || lastSlash == len(repoPath)-1 {
		return generated.ArchiveRepositoryRef{}, errors.New("expected provider|host/repo_path")
	}
	return generated.ArchiveRepositoryRef{
		Provider: provider, PlatformHost: host, Owner: repoPath[:lastSlash],
		Name: repoPath[lastSlash+1:], RepoPath: repoPath,
	}, nil
}

func parseArchiveReportRange(now time.Time, days int, startValue, endValue string) (time.Time, time.Time, error) {
	startValue = strings.TrimSpace(startValue)
	endValue = strings.TrimSpace(endValue)
	if days != 0 && (startValue != "" || endValue != "") {
		return time.Time{}, time.Time{}, errors.New("--days and --start/--end are mutually exclusive")
	}
	if days != 0 {
		if days < 0 {
			return time.Time{}, time.Time{}, errors.New("--days must be positive")
		}
		end := now.UTC()
		return end.Add(-time.Duration(days) * 24 * time.Hour), end, nil
	}
	if startValue == "" || endValue == "" {
		return time.Time{}, time.Time{}, errors.New("provide --days or both --start and --end")
	}
	startDate, startDateErr := time.Parse(time.DateOnly, startValue)
	endDate, endDateErr := time.Parse(time.DateOnly, endValue)
	dateOnly := startDateErr == nil && endDateErr == nil
	if (startDateErr == nil) != (endDateErr == nil) {
		return time.Time{}, time.Time{}, errors.New("--start and --end must use the same form: dates or RFC3339")
	}
	var start, end time.Time
	if dateOnly {
		start = startDate.UTC()
		end = endDate.UTC().Add(24 * time.Hour)
	} else {
		var err error
		start, err = time.Parse(time.RFC3339, startValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("parse --start as UTC date or RFC3339: %w", err)
		}
		end, err = time.Parse(time.RFC3339, endValue)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("parse --end as UTC date or RFC3339: %w", err)
		}
		start = start.UTC()
		end = end.UTC()
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("archive report start must precede end")
	}
	return start, end, nil
}

func archiveReportFromAPI(input generated.ArchiveReportResponse) (report.Model, error) {
	if input.ReportSchema != report.Schema {
		return report.Model{}, fmt.Errorf(
			"unsupported archive report schema %q (want %q)",
			input.ReportSchema, report.Schema,
		)
	}
	model := report.Model{
		Schema: input.ReportSchema,
		Start:  input.Start.UTC(),
		End:    input.End.UTC(),
	}
	if input.Repositories != nil {
		model.Repositories = make([]report.Repository, len(input.Repositories))
		for i, item := range input.Repositories {
			model.Repositories[i] = report.Repository{
				Repository: archiveReportRepositoryRefFromAPI(item.Repository),
				Coverage:   archiveReportCoverageFromAPI(item.Coverage),
				Counts:     archiveReportCountsFromAPI(item.Counts),
			}
		}
	} else {
		model.Repositories = []report.Repository{}
	}
	model.Totals = archiveReportCountsFromAPI(input.Totals)
	if input.Contributors != nil {
		model.Contributors = make([]report.Contributor, len(input.Contributors))
		for i, item := range input.Contributors {
			model.Contributors[i] = report.Contributor{
				Provider: item.Provider, PlatformHost: item.PlatformHost,
				Login: item.Login, Counts: archiveReportCountsFromAPI(item.Counts),
			}
		}
	} else {
		model.Contributors = []report.Contributor{}
	}
	if input.Activity != nil {
		model.Activity = make([]report.Activity, len(*input.Activity))
		for i, item := range *input.Activity {
			kind := report.ActivityKind(item.Kind)
			if !validArchiveActivityKind(kind) {
				return report.Model{}, fmt.Errorf("unknown activity kind %q", item.Kind)
			}
			model.Activity[i] = report.Activity{
				Repository: archiveReportRepositoryRefFromAPI(item.Repository), Kind: kind,
				ItemNumber: int(item.ItemNumber), ProviderExternalID: item.ProviderExternalId,
				Title: item.Title, Author: item.Author, Actor: archiveOptionalString(item.Actor),
				OccurredAt: item.OccurredAt.UTC(), Body: item.Body, URL: item.Url,
				Comments:       archiveOptionalInt(item.Comments),
				Additions:      archiveOptionalInt(item.Additions),
				Deletions:      archiveOptionalInt(item.Deletions),
				FilesChanged:   archiveOptionalIntPointer(item.FilesChanged),
				MergeCommitSHA: archiveOptionalString(item.MergeCommitSha),
			}
		}
	}
	return model, nil
}

func archiveOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func archiveOptionalInt(value *int64) int {
	if value == nil {
		return 0
	}
	return int(*value)
}

func archiveOptionalIntPointer(value *int64) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}

func archiveReportRepositoryRefFromAPI(input generated.ArchiveRepositoryRef) report.RepositoryRef {
	return report.RepositoryRef{
		Provider: input.Provider, PlatformHost: input.PlatformHost, Owner: input.Owner,
		Name: input.Name, RepoPath: input.RepoPath,
	}
}

func archiveReportCountsFromAPI(input generated.ArchiveReportCountsResponse) report.Counts {
	return report.Counts{
		IssuesOpened: int(input.IssuesOpened), IssuesClosed: int(input.IssuesClosed),
		MergeRequestsOpened:  int(input.MergeRequestsOpened),
		MergeRequestsMerged:  int(input.MergeRequestsMerged),
		OrdinaryComments:     int(input.OrdinaryComments),
		ReviewsSubmitted:     int(input.ReviewsSubmitted),
		InlineReviewComments: int(input.InlineReviewComments),
	}
}

func archiveReportCoverageFromAPI(input generated.ArchiveReportCoverageResponse) report.Coverage {
	phases := []string{}
	if input.ActivePhases != nil {
		phases = append(phases, input.ActivePhases...)
	}
	return report.Coverage{
		Status: string(input.Status), ActivePhases: phases,
		CollectionMode: string(input.CollectionMode), OperatorState: string(input.OperatorState),
		Issues: string(input.Issues), MergeRequests: string(input.MergeRequests),
		Comments: string(input.Comments), Reviews: string(input.Reviews),
		InlineComments: string(input.InlineComments), InitialCompletedAt: input.InitialCompletedAt,
		MaintenanceSucceededAt: input.MaintenanceSucceededAt,
		BudgetWaitUntil:        input.BudgetWaitUntil, ArchivedItems: int(input.ArchivedItems),
		UnsupportedItems: int(input.UnsupportedItems), InaccessibleItems: int(input.InaccessibleItems),
	}
}

func validArchiveActivityKind(kind report.ActivityKind) bool {
	switch kind {
	case report.ActivityIssue, report.ActivityIssueClosed,
		report.ActivityMergeRequest, report.ActivityMergeRequestMerged,
		report.ActivityOrdinaryComment,
		report.ActivityReview, report.ActivityInlineReviewComment:
		return true
	default:
		return false
	}
}

func renderArchiveReport(model report.Model, format string) (string, error) {
	if format == "markdown" {
		return report.RenderMarkdown(model)
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render archive report JSON: %w", err)
	}
	return string(data) + "\n", nil
}

func writeArchiveJSON(output io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("render archive JSON: %w", err)
	}
	_, err = fmt.Fprintf(output, "%s\n", data)
	return err
}

func writeArchiveOutput(path string, contents string) (err error) {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create archive output temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err = io.WriteString(temp, contents); err != nil {
		return fmt.Errorf("write archive output temp file: %w", err)
	}
	if err = temp.Sync(); err != nil {
		return fmt.Errorf("sync archive output temp file: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close archive output temp file: %w", err)
	}
	if err = os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace archive output: %w", err)
	}
	return nil
}

func archiveAPIProblem(operation string, status int, problem *generated.ProblemError) error {
	if problem == nil {
		return fmt.Errorf("%s failed with HTTP %d", operation, status)
	}
	if problem.Code == generated.PayloadTooLarge && archiveProblemReason(problem) == "reportTooLarge" {
		return errors.New("archive report is too large; narrow the UTC range or repository scope")
	}
	if problem.Details != nil && len(*problem.Details) > 0 {
		details, err := json.Marshal(*problem.Details)
		if err == nil {
			return fmt.Errorf("%s failed with HTTP %d (%s; details=%s)", operation, status, problem.Code, details)
		}
	}
	return fmt.Errorf("%s failed with HTTP %d (%s)", operation, status, problem.Code)
}

func archiveProblemReason(problem *generated.ProblemError) string {
	if problem.Details == nil {
		return ""
	}
	reason, _ := (*problem.Details)["reason"].(string)
	return reason
}
