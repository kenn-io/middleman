package report

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

func RenderMarkdown(model Model) (string, error) {
	var out strings.Builder
	out.WriteString("# Activity archive\n\nUTC range: ")
	out.WriteString(formatTime(model.Start))
	out.WriteString(" to ")
	out.WriteString(formatTime(model.End))
	out.WriteString(" (exclusive)\n\n## Totals\n\n")
	writeCounts(&out, model.Totals)

	out.WriteString("\n## Repositories\n")
	for _, repo := range model.Repositories {
		fmt.Fprintf(&out, "\n### %s\n\nStatus: %s\n\n", formatRepository(repo.Repository), inline(repo.Coverage.Status))
		writeCounts(&out, repo.Counts)
		fmt.Fprintf(&out, "- Issue coverage: %s\n", inline(repo.Coverage.Issues))
		fmt.Fprintf(&out, "- Pull or merge request coverage: %s\n", inline(repo.Coverage.MergeRequests))
		fmt.Fprintf(&out, "- Comment coverage: %s\n", inline(repo.Coverage.Comments))
		fmt.Fprintf(&out, "- Review coverage: %s\n", inline(repo.Coverage.Reviews))
		fmt.Fprintf(&out, "- Inline comment coverage: %s\n", inline(repo.Coverage.InlineComments))
	}

	out.WriteString("\n## Contributors\n\n")
	out.WriteString("| Provider | Host | Login | Issues Opened | Issues Closed | Pull/Merge Requests Opened | Pull/Merge Requests Merged | Comments | Reviews | Inline Comments | Total |\n")
	out.WriteString("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, contributor := range model.Contributors {
		login := contributor.Login
		if login == "" {
			login = "(unknown)"
		}
		fmt.Fprintf(
			&out, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			tableCell(contributor.Provider), tableCell(contributor.PlatformHost), tableCell(login),
			contributor.Counts.IssuesOpened, contributor.Counts.IssuesClosed,
			contributor.Counts.MergeRequestsOpened, contributor.Counts.MergeRequestsMerged,
			contributor.Counts.OrdinaryComments, contributor.Counts.ReviewsSubmitted,
			contributor.Counts.InlineReviewComments, contributor.Counts.TotalActivity(),
		)
	}

	if len(model.Activity) > 0 {
		out.WriteString("\n## Activity\n")
		for _, activity := range model.Activity {
			fmt.Fprintf(&out, "\n### %s\n\n", activityHeading(activity))
			fmt.Fprintf(&out, "- Repository: %s\n", formatRepository(activity.Repository))
			author := activity.Author
			if author == "" {
				author = "(unknown)"
			}
			fmt.Fprintf(&out, "- Author: %s\n", providerInline(author))
			if activity.Actor != "" {
				fmt.Fprintf(&out, "- Actor: %s\n", providerInline(activity.Actor))
			}
			fmt.Fprintf(&out, "- Time: %s\n", formatTime(activity.OccurredAt))
			if activity.Comments != 0 {
				fmt.Fprintf(&out, "- Comments: %d\n", activity.Comments)
			}
			if activity.Additions != 0 || activity.Deletions != 0 {
				fmt.Fprintf(&out, "- Changes: +%d -%d\n", activity.Additions, activity.Deletions)
			}
			if activity.FilesChanged != nil {
				fmt.Fprintf(&out, "- Files changed: %d\n", *activity.FilesChanged)
			}
			if activity.MergeCommitSHA != "" {
				fmt.Fprintf(&out, "- Merge commit: %s\n", providerInline(activity.MergeCommitSHA))
			}
			if activity.URL != "" {
				fmt.Fprintf(&out, "- URL: %s\n", providerURL(activity.URL))
			}
			if activity.Body != "" {
				fmt.Fprintf(&out, "\n%s\n", providerBlock(activity.Body))
			}
		}
	}
	return out.String(), nil
}

func writeCounts(out *strings.Builder, counts Counts) {
	fmt.Fprintf(out, "- Issues opened: %d\n", counts.IssuesOpened)
	fmt.Fprintf(out, "- Issues closed: %d\n", counts.IssuesClosed)
	fmt.Fprintf(out, "- Pull or merge requests opened: %d\n", counts.MergeRequestsOpened)
	fmt.Fprintf(out, "- Pull or merge requests merged: %d\n", counts.MergeRequestsMerged)
	fmt.Fprintf(out, "- Ordinary comments added: %d\n", counts.OrdinaryComments)
	fmt.Fprintf(out, "- Reviews submitted: %d\n", counts.ReviewsSubmitted)
	fmt.Fprintf(out, "- Inline review comments added: %d\n", counts.InlineReviewComments)
	fmt.Fprintf(out, "- Total activity: %d\n", counts.TotalActivity())
}

func activityHeading(activity Activity) string {
	title := providerInline(activity.Title)
	switch activity.Kind {
	case ActivityIssue:
		return fmt.Sprintf("Issue #%d: %s", activity.ItemNumber, title)
	case ActivityIssueClosed:
		return fmt.Sprintf("Issue #%d closed: %s", activity.ItemNumber, title)
	case ActivityMergeRequest:
		return fmt.Sprintf("Pull or merge request #%d: %s", activity.ItemNumber, title)
	case ActivityMergeRequestMerged:
		return fmt.Sprintf("Pull or merge request #%d merged: %s", activity.ItemNumber, title)
	case ActivityOrdinaryComment:
		return fmt.Sprintf("Ordinary comment on #%d: %s", activity.ItemNumber, title)
	case ActivityReview:
		return fmt.Sprintf("Review on #%d: %s", activity.ItemNumber, title)
	case ActivityInlineReviewComment:
		return fmt.Sprintf("Inline review comment on #%d: %s", activity.ItemNumber, title)
	default:
		return fmt.Sprintf("Activity on #%d: %s", activity.ItemNumber, title)
	}
}

func formatRepository(ref RepositoryRef) string {
	return providerInline(ref.Provider) + " · " + providerInline(ref.PlatformHost) + " · " + providerInline(ref.RepoPath)
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339) }

func inline(value string) string {
	return strings.Join(strings.Fields(normalizeLines(value)), " ")
}

func tableCell(value string) string {
	return providerInline(value)
}

func providerInline(value string) string { return escapeMarkdown(inline(value)) }

func providerBlock(value string) string {
	value = normalizeLines(value)
	fence := strings.Repeat("`", max(3, longestRun(value, '`')+1))
	return fence + "\n" + value + "\n" + fence
}

func longestRun(value string, target rune) int {
	longest := 0
	current := 0
	for _, r := range value {
		if r != target {
			current = 0
			continue
		}
		current++
		longest = max(longest, current)
	}
	return longest
}

func providerURL(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\", "&", "&amp;", "<", "&lt;", ">", "&gt;",
		"[", "\\[", "]", "\\]",
	).Replace(inline(value))
}

func escapeMarkdown(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\", "&", "&amp;", "<", "&lt;", ">", "&gt;",
		"`", "\\`", "*", "\\*", "_", "\\_", "{", "\\{", "}", "\\}",
		"[", "\\[", "]", "\\]",
		"#", "\\#", "+", "\\+", "-", "\\-", "!", "\\!", "|", "\\|",
	).Replace(value)
}

func normalizeLines(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
