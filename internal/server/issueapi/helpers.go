package issueapi

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/platform"
)

func (s *Handler) lookupRepoMap(ctx context.Context) (map[int64]db.Repo, error) {
	repos, err := s.db.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	if s.filterRepos != nil {
		repos = s.filterRepos(repos)
	}
	lookup := make(map[int64]db.Repo, len(repos))
	for _, repo := range repos {
		lookup[repo.ID] = repo
	}
	return lookup, nil
}

func parseRepoFilter(repo string) (provider, platformHost, repoPath string) {
	repo = strings.Trim(repo, "/ ")
	providerPart, hostedPath, ok := strings.Cut(repo, "|")
	if !ok {
		return "", "", ""
	}
	provider = strings.ToLower(strings.TrimSpace(providerPart))
	if _, ok := platform.MetadataFor(platform.Kind(provider)); !ok {
		return "", "", ""
	}
	parts := strings.Split(strings.Trim(hostedPath, "/ "), "/")
	if len(parts) < 2 {
		return "", "", ""
	}
	return provider, parts[0], strings.Join(parts[1:], "/")
}

func parseRepoFilters(value string) []db.RepoFilter {
	parts := strings.Split(value, ",")
	filters := make([]db.RepoFilter, 0, len(parts))
	for _, part := range parts {
		provider, host, repoPath := parseRepoFilter(part)
		if repoPath != "" {
			filters = append(filters, db.RepoFilter{Platform: provider, PlatformHost: host, RepoPath: repoPath})
		}
	}
	return filters
}

func hasInvalidRepoFilter(value string) bool {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.Trim(part, "/ ")
		if part == "" {
			continue
		}
		_, _, repoPath := parseRepoFilter(part)
		if repoPath == "" {
			return true
		}
	}
	return false
}

func formatUTCRFC3339(value time.Time) string { return value.UTC().Format(time.RFC3339) }

func issueResponseModel(issue db.Issue) db.Issue {
	issue.WorkflowStatus = normalizeWorkflowStatus(string(issue.WorkflowStatus), "issue_id", issue.ID)
	return issue
}

func normalizeWorkflowStatus(status string, logAttrs ...any) db.KanbanStatus {
	switch db.KanbanStatus(status) {
	case db.KanbanStatusNew, db.KanbanStatusReviewing, db.KanbanStatusWaiting, db.KanbanStatusAwaitingMerge:
		return db.KanbanStatus(status)
	case "":
		return db.KanbanStatusNew
	default:
		attrs := append([]any{"status", status}, logAttrs...)
		slog.Warn("normalizing unexpected workflow status in response", attrs...)
		return db.KanbanStatusNew
	}
}

func labelCatalogStale(freshness db.LabelCatalogFreshness, now time.Time) bool {
	return freshness.CheckedAt == nil || freshness.CheckedAt.Before(now.Add(-10*time.Minute))
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
