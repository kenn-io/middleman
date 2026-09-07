package workspaceapi

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/fleet"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/platform"
	gitcmd "go.kenn.io/kit/git/cmd"
)

func expandHomeCWD(cwd string) string {
	if cwd != "~" && !strings.HasPrefix(cwd, "~/") {
		return cwd
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return cwd
	}
	if cwd == "~" {
		return home
	}
	return filepath.Join(home, cwd[2:])
}

func (s *Handler) lookupRepoByProviderRoute(
	ctx context.Context, provider, platformHost, owner, name string,
) (*db.Repo, error) {
	if s.lookupRepo != nil {
		return s.lookupRepo(ctx, provider, platformHost, owner, name)
	}
	owner = strings.Trim(owner, "/ ")
	name = strings.Trim(name, "/ ")
	if owner == "" || name == "" {
		return nil, httpapi.ErrRepoPathRequired
	}
	return s.resolver.Lookup(ctx, provider, platformHost, owner+"/"+name)
}

func providerRouteLookupError(err error) error {
	if errors.Is(err, httpapi.ErrRepoPathRequired) {
		return httpapi.BadRequest(httpapi.CodeBadRequest, err.Error(), nil)
	}
	if errors.Is(err, httpapi.ErrRepoNotFound) {
		return httpapi.NotFound(httpapi.CodeRepoNotFound, "repo not found", nil)
	}
	if strings.Contains(err.Error(), "platform_host is required") ||
		strings.Contains(err.Error(), "unsupported platform") {
		return httpapi.BadRequest(httpapi.CodeBadRequest, err.Error(), nil)
	}
	return httpapi.Internal("get repo failed")
}

func repoProviderKind(repo db.Repo) platform.Kind {
	if strings.TrimSpace(repo.Platform) == "" {
		return platform.KindGitHub
	}
	return platform.Kind(repo.Platform)
}

func repoProviderHost(repo db.Repo) string {
	if strings.TrimSpace(repo.PlatformHost) != "" {
		return repo.PlatformHost
	}
	if host, ok := platform.DefaultHost(repoProviderKind(repo)); ok {
		return host
	}
	return platform.DefaultGitHubHost
}

func (s *Handler) repoRefFromParts(
	provider, host, owner, name string,
) httpapi.RepoRefResponse {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = string(platform.KindGitHub)
	}
	resp := httpapi.RepoRefResponse{
		Provider: provider, PlatformHost: host,
		RepoPath: owner + "/" + name, Owner: owner, Name: name,
	}
	if s.resolver != nil {
		resp.Capabilities = s.resolver.Capabilities(platform.Kind(provider), host)
	}
	return resp
}

func (s *Handler) recomputeWorktreeLinksNow(ctx context.Context) {
	if s.recomputeWorktreeLinks != nil {
		s.recomputeWorktreeLinks(ctx)
	}
}

func normPath(path string) string { return fleet.NormPath(path) }

func gitDiscoveryOutput(
	ctx context.Context, dir string, args ...string,
) (string, error) {
	out, err := gitcmd.New().Output(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeRouteProvider(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("provider is required")
	}
	kind, err := platform.NormalizeKind(raw)
	if err != nil {
		return "", err
	}
	return string(kind), nil
}

// mrHeadRepoKindSameRepo, mrHeadRepoKindFork, and mrHeadRepoKindUnknown are
// the tri-state values exposed on workspaceResponse.MRHeadRepoKind for
// pull_request workspaces, mirroring db.Workspace.MRHeadRepo's nil/empty/set
// states. Non-pull_request workspaces get the zero value ("") so the field
// is omitted from the wire response.
const (
	mrHeadRepoKindSameRepo = "same_repo"
	mrHeadRepoKindFork     = "fork"
	mrHeadRepoKindUnknown  = "unknown"
)

// mrHeadRepoKind exposes informational head-repository classification to API
// clients. It does not authorize launches; an explicit user-selected launch
// goes through the ordinary manual launch boundary for every classification.
func mrHeadRepoKind(itemType string, mrHeadRepo *string) string {
	if itemType != db.WorkspaceItemTypePullRequest {
		return ""
	}
	switch {
	case mrHeadRepo == nil:
		return mrHeadRepoKindSameRepo
	case *mrHeadRepo == "":
		return mrHeadRepoKindUnknown
	default:
		return mrHeadRepoKindFork
	}
}

func toWorkspaceResponse(summary *db.WorkspaceSummary) workspaceResponse {
	var itemLastActivityAt *string
	if summary.ItemLastActivityAt != nil {
		formatted := summary.ItemLastActivityAt.UTC().Format(time.RFC3339)
		itemLastActivityAt = &formatted
	}
	associatedPRNumber := summary.AssociatedPRNumber
	if !summary.AssociatedPRVisible {
		associatedPRNumber = nil
	}
	headRepoKind := ""
	if summary.SourceItemVisible {
		headRepoKind = mrHeadRepoKind(summary.ItemType, summary.MRHeadRepo)
	}
	return workspaceResponse{
		ID: summary.ID,
		Repo: httpapi.RepoRefResponse{
			Provider: summary.Platform, PlatformHost: summary.PlatformHost,
			PlatformRepoID: summary.RepoPlatformID,
			RepoPath:       summary.RepoOwner + "/" + summary.RepoName,
			Owner:          summary.RepoOwner, Name: summary.RepoName,
		},
		PlatformHost:       summary.PlatformHost,
		RepoOwner:          summary.RepoOwner,
		RepoName:           summary.RepoName,
		ItemType:           summary.ItemType,
		ItemNumber:         summary.ItemNumber,
		ItemKey:            summary.ItemKey,
		GitHeadRef:         summary.GitHeadRef,
		WorktreePath:       summary.WorktreePath,
		TmuxSession:        summary.TmuxSession,
		Status:             summary.Status,
		EnrichmentStatus:   workspaceEnrichmentNotApplicable,
		TmuxActivitySource: tmuxActivitySourceUnknown,
		ErrorMessage:       summary.ErrorMessage,
		CreatedAt:          summary.CreatedAt.UTC().Format(time.RFC3339),
		ItemLastActivityAt: itemLastActivityAt,
		MRTitle:            summary.MRTitle,
		MRState:            summary.MRState,
		MRIsDraft:          summary.MRIsDraft,
		MRCIStatus:         summary.MRCIStatus,
		MRReviewDecision:   summary.MRReviewDecision,
		MRAdditions:        summary.MRAdditions,
		MRDeletions:        summary.MRDeletions,
		AssociatedPRNumber: associatedPRNumber,
		Kata:               summary.KataMetadata,
		MRHeadRepoKind:     headRepoKind,
	}
}

func formatUTCRFC3339(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

const maxFilePreviewBytes int64 = 4 * 1024 * 1024

func previewMediaType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".jsonc":
		return "application/jsonc; charset=utf-8"
	case ".toml":
		return "application/toml; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	}
	if mediaType := mime.TypeByExtension(filepath.Ext(path)); mediaType != "" {
		return mediaType
	}
	return http.DetectContentType(data)
}

func logWebsocketDebug(msg string, args ...any) {
	if !websocketDebugEnabled() {
		return
	}
	slog.Debug(msg, args...)
}

func websocketDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KENN_FORGE_WS_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

const runtimeSessionCleanupTimeout = 2 * time.Second
