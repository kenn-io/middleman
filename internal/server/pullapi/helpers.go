package pullapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
)

type repoNumberPathRef struct {
	repoID       int64
	owner        string
	name         string
	number       int
	platformHost string
}

func buildRepoLookup(repos []db.Repo) map[int64]db.Repo {
	lookup := make(map[int64]db.Repo, len(repos))
	for _, repo := range repos {
		lookup[repo.ID] = repo
	}
	return lookup
}

func (s *Handler) lookupRepoMap(ctx context.Context) (map[int64]db.Repo, error) {
	repos, err := s.db.ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	if s.filterRepos != nil {
		repos = s.filterRepos(repos)
	}
	return buildRepoLookup(repos), nil
}

// visibleMergeRequest is the public pull API's read boundary. Provider sync
// paths deliberately use the unfiltered getter so retained archive evidence
// can still be repaired.
func (s *Handler) visibleMergeRequest(
	ctx context.Context, repoID int64, number int,
) (*db.MergeRequest, error) {
	return s.db.GetVisibleMergeRequestByRepoIDAndNumber(ctx, repoID, number)
}

// requireVisibleMergeRequest is the public pull mutation boundary. It must run
// before provider access so a retained removed-upstream row cannot cause an
// external side effect.
func (s *Handler) requireVisibleMergeRequest(
	ctx context.Context, repo *db.Repo, number int,
) (*db.MergeRequest, error) {
	mr, err := s.visibleMergeRequest(ctx, repo.ID, number)
	if err != nil {
		return nil, httpapi.Internal("get pull request failed: " + err.Error())
	}
	if mr == nil {
		return nil, httpapi.NotFound(
			httpapi.CodePullNotFound,
			fmt.Sprintf(
				"pull request %s/%s#%d on %s not found",
				repo.Owner, repo.Name, number, repo.PlatformHost,
			),
			nil,
		)
	}
	return mr, nil
}

func (s *Handler) visibleDiffSHAs(
	ctx context.Context, repoID int64, number int,
) (*db.DiffSHAs, error) {
	mr, err := s.visibleMergeRequest(ctx, repoID, number)
	if err != nil || mr == nil {
		return nil, err
	}
	return &db.DiffSHAs{
		PlatformHeadSHA: mr.PlatformHeadSHA,
		PlatformBaseSHA: mr.PlatformBaseSHA,
		DiffHeadSHA:     mr.DiffHeadSHA,
		DiffBaseSHA:     mr.DiffBaseSHA,
		MergeBaseSHA:    mr.MergeBaseSHA,
		State:           string(mr.State),
	}, nil
}

// filterConfiguredRepos returns only repos that are currently tracked.
func (s *Handler) lookupMRID(ctx context.Context, ref repoNumberPathRef) (int64, error) {
	mr, err := s.visibleMergeRequest(
		ctx, ref.repoID, ref.number,
	)
	if err != nil {
		return 0, err
	}
	if mr == nil {
		return 0, fmt.Errorf(
			"pull request %s/%s#%d on %s not found",
			ref.owner, ref.name, ref.number, ref.platformHost,
		)
	}
	return mr.ID, nil
}

// lookupIssueID resolves the internal issue id from the common route tuple.
func parseRepoFilter(repo string) (provider, platformHost, owner, name, repoPath string) {
	repo = strings.Trim(repo, "/ ")
	if providerPart, hostedPath, ok := strings.Cut(repo, "|"); ok {
		provider := strings.ToLower(strings.TrimSpace(providerPart))
		if _, ok := platform.MetadataFor(platform.Kind(provider)); !ok {
			return "", "", "", "", ""
		}
		parts := strings.Split(strings.Trim(hostedPath, "/ "), "/")
		if len(parts) < 2 {
			return "", "", "", "", ""
		}
		return provider, parts[0], "", "", strings.Join(parts[1:], "/")
	}
	return "", "", "", "", ""
}

func parseRepoFilters(repo string) []db.RepoFilter {
	parts := strings.Split(repo, ",")
	filters := make([]db.RepoFilter, 0, len(parts))
	for _, part := range parts {
		provider, platformHost, owner, name, repoPath := parseRepoFilter(part)
		if repoPath != "" {
			filters = append(filters, db.RepoFilter{
				Platform:     provider,
				PlatformHost: platformHost,
				RepoPath:     repoPath,
			})
		} else if owner != "" {
			filters = append(filters, db.RepoFilter{
				Platform:     provider,
				PlatformHost: platformHost,
				RepoOwner:    owner,
				RepoName:     name,
			})
		}
	}
	return filters
}

func hasInvalidRepoFilter(repo string) bool {
	for part := range strings.SplitSeq(repo, ",") {
		part = strings.Trim(part, "/ ")
		if part == "" {
			continue
		}
		_, _, owner, _, repoPath := parseRepoFilter(part)
		if owner == "" && repoPath == "" {
			return true
		}
	}
	return false
}

func formatUTCRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func toWorktreeLinkResponses(
	links []db.WorktreeLink,
	hostKey string,
) []worktreeLinkResponse {
	out := make([]worktreeLinkResponse, len(links))
	for i, l := range links {
		out[i] = worktreeLinkResponse{
			HostKey:        hostKey,
			WorktreeKey:    l.WorktreeKey,
			WorktreePath:   l.WorktreePath,
			WorktreeBranch: l.WorktreeBranch,
		}
	}
	return out
}

// indexWorktreeLinksByMR groups worktree link responses by
// merge request ID.
func indexWorktreeLinksByMR(
	links []db.WorktreeLink,
	hostKey string,
) map[int64][]worktreeLinkResponse {
	m := make(map[int64][]worktreeLinkResponse)
	for _, l := range links {
		m[l.MergeRequestID] = append(
			m[l.MergeRequestID],
			worktreeLinkResponse{
				HostKey:        hostKey,
				WorktreeKey:    l.WorktreeKey,
				WorktreePath:   l.WorktreePath,
				WorktreeBranch: l.WorktreeBranch,
			},
		)
	}
	return m
}
