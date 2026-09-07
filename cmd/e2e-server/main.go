package main

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gh "github.com/google/go-github/v89/github"
	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/db"
	"go.kenn.io/forge/internal/federation"
	"go.kenn.io/forge/internal/federationauth"
	"go.kenn.io/forge/internal/gitclone"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/procutil"
	"go.kenn.io/forge/internal/profiler"
	"go.kenn.io/forge/internal/ptyowner"
	"go.kenn.io/forge/internal/server"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/server/workspaceapi"
	"go.kenn.io/forge/internal/stacks"
	"go.kenn.io/forge/internal/testutil"
	"go.kenn.io/forge/internal/testutil/federationtest"
	"go.kenn.io/forge/internal/tokenauth"
	"go.kenn.io/forge/internal/web"
	"go.kenn.io/forge/internal/workspace"
	"go.kenn.io/forge/platform"
	platformgithub "go.kenn.io/forge/platform/github"
	gitcmd "go.kenn.io/kit/git/cmd"
	oteltelemetry "go.kenn.io/kit/telemetry"
)

// defaultRoborevEndpoint is the address the e2e server points the
// roborev proxy at when -roborev is not provided. It is deliberately
// an unbindable loopback port so direct playwright runs fail closed
// (the proxy returns 502) instead of silently forwarding test
// traffic to a real local roborev daemon (typically at
// 127.0.0.1:7373). The runner script (scripts/run-roborev-e2e.sh)
// always passes -roborev explicitly to the dockerized seeded daemon.
const defaultRoborevEndpoint = "http://127.0.0.1:1"

const e2eTmuxDirEnv = "PLAYWRIGHT_E2E_TMUX_DIR"

const (
	e2eStandaloneNodeID = "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0"
	e2eHubNodeID        = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	e2eSpokeANodeID     = "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
	e2eSpokeBNodeID     = "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"
)

func main() {
	port := flag.Int("port", 0, "port to listen on (0 selects a random free port)")
	roborev := flag.String(
		"roborev", defaultRoborevEndpoint,
		"roborev daemon endpoint",
	)
	defaultPlatformHost := flag.String(
		"default-platform-host", "github.com",
		"default platform host for seeded config",
	)
	federatedForges := flag.Bool(
		"federated-forges", false,
		"serve an isolated hub and two federation spokes",
	)
	visibleImportedModes := flag.Bool(
		"visible-imported-modes", false,
		"show imported app modes in the seeded config",
	)
	providerCollision := flag.Bool(
		"provider-collision",
		false,
		"seed same host/repo_path under multiple providers",
	)
	serverInfoFile := flag.String(
		"server-info-file", "",
		"path to write discovered server port info as JSON",
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	var err error
	if *federatedForges {
		err = runFederatedForgesE2E(ctx, *roborev, *serverInfoFile)
	} else {
		err = run(
			ctx,
			*port,
			*roborev,
			*serverInfoFile,
			*defaultPlatformHost,
			*visibleImportedModes,
			*providerCollision,
		)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type e2eServerInfo struct {
	Host       string             `json:"host"`
	Port       int                `json:"port"`
	BaseURL    string             `json:"base_url"`
	PID        int                `json:"pid"`
	ConfigPath string             `json:"config_path"`
	PprofAddr  string             `json:"pprof_addr,omitempty"`
	NodeID     string             `json:"node_id"`
	Federation *e2eFederationInfo `json:"federation,omitempty"`
}

type e2eFederationInfo struct {
	HubURL       string `json:"hub_url"`
	SpokeAURL    string `json:"spoke_a_url"`
	SpokeBURL    string `json:"spoke_b_url"`
	ControlURL   string `json:"control_url"`
	HubToken     string `json:"hub_token"`
	SpokeAToken  string `json:"spoke_a_token"`
	SpokeBToken  string `json:"spoke_b_token"`
	HubNodeID    string `json:"hub_node_id"`
	SpokeANodeID string `json:"spoke_a_node_id"`
	SpokeBNodeID string `json:"spoke_b_node_id"`
}

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) {
	return string(s), nil
}

func (s staticTokenSource) Invalidate(string) {}

func (s staticTokenSource) Descriptor() tokenauth.Descriptor {
	return tokenauth.Descriptor{Key: tokenauth.Key{Platform: "github", Host: "github.com"}}
}

type e2eStaticProvider struct {
	kind        platform.Kind
	host        string
	caps        platform.Capabilities
	repos       []platform.Repository
	issue       platform.Issue
	issueEvents []platform.IssueEvent
}

func (p e2eStaticProvider) Platform() platform.Kind {
	return p.kind
}

func (p e2eStaticProvider) Host() string {
	return p.host
}

func (p e2eStaticProvider) Capabilities() platform.Capabilities {
	return p.caps
}

func (p e2eStaticProvider) AuthenticatedUser(
	context.Context, platform.RepoRef,
) (string, error) {
	return "fixture-viewer", nil
}

func (p e2eStaticProvider) GetRepository(
	_ context.Context,
	ref platform.RepoRef,
) (platform.Repository, error) {
	for _, repo := range p.repos {
		if repo.Ref.RepoPath == ref.RepoPath ||
			(repo.Ref.Owner == ref.Owner && repo.Ref.Name == ref.Name) {
			return repo, nil
		}
	}
	return platform.Repository{}, platform.ErrNotFound
}

func (p e2eStaticProvider) ListRepositories(
	_ context.Context,
	owner string,
	_ platform.RepositoryListOptions,
) ([]platform.Repository, error) {
	repos := make([]platform.Repository, 0, len(p.repos))
	for _, repo := range p.repos {
		if strings.EqualFold(repo.Ref.Owner, owner) {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

func (p e2eStaticProvider) ListIssuesPage(
	_ context.Context,
	ref platform.RepoRef,
	query platform.ItemPageQuery,
) (platform.Page[platform.Issue], error) {
	if err := platform.ValidateItemPageQuery(query); err != nil {
		return platform.Page[platform.Issue]{}, err
	}
	if ref.RepoPath != p.issue.Repo.RepoPath {
		return platform.Page[platform.Issue]{Exhausted: true}, nil
	}
	return platform.Page[platform.Issue]{
		Items: []platform.Issue{p.issue}, Exhausted: true,
	}, nil
}

func (p e2eStaticProvider) ListOpenIssues(
	_ context.Context,
	ref platform.RepoRef,
) ([]platform.Issue, error) {
	if ref.RepoPath == p.issue.Repo.RepoPath && p.issue.State == "open" {
		return []platform.Issue{p.issue}, nil
	}
	return nil, nil
}

func (p e2eStaticProvider) GetIssue(
	_ context.Context,
	ref platform.RepoRef,
	number int,
) (platform.Issue, error) {
	if ref.RepoPath == p.issue.Repo.RepoPath && number == p.issue.Number {
		return p.issue, nil
	}
	return platform.Issue{}, platform.ErrNotFound
}

func (p e2eStaticProvider) ListIssueEvents(
	_ context.Context,
	ref platform.RepoRef,
	number int,
) ([]platform.IssueEvent, error) {
	if ref.RepoPath == p.issue.Repo.RepoPath && number == p.issue.Number {
		return slices.Clone(p.issueEvents), nil
	}
	return nil, nil
}

type globRefreshContextKey struct{}

func e2eGit(ctx context.Context, dir string, args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("git: no args")
	}
	cmd := gitcmd.New().Command(ctx, dir, args...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_DATE=2026-04-28T12:00:00Z",
		"GIT_COMMITTER_DATE=2026-04-28T12:00:00Z",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, stderr.String())
	}
	return nil
}

func createBareRepoFixture(ctx context.Context, tmpDir, host, owner, name string) (string, error) {
	workDir := filepath.Join(tmpDir, "fixture-work", host, owner, name)
	barePath := filepath.Join(tmpDir, "clones", host, owner, name+".git")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir fixture workdir: %w", err)
	}
	if err := e2eGit(ctx, workDir, "init", "-b", "main"); err != nil {
		return "", fmt.Errorf("init fixture repo: %w", err)
	}
	if err := e2eGit(ctx, workDir, "config", "user.email", "e2e@example.com"); err != nil {
		return "", fmt.Errorf("config fixture repo email: %w", err)
	}
	if err := e2eGit(ctx, workDir, "config", "user.name", "E2E Fixture"); err != nil {
		return "", fmt.Errorf("config fixture repo name: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(workDir, "README.md"),
		[]byte("# GitLab fixture\n"),
		0o644,
	); err != nil {
		return "", fmt.Errorf("write fixture file: %w", err)
	}
	if err := e2eGit(ctx, workDir, "add", "README.md"); err != nil {
		return "", fmt.Errorf("stage fixture repo: %w", err)
	}
	if err := e2eGit(ctx, workDir, "commit", "-m", "fixture: seed gitlab repo"); err != nil {
		return "", fmt.Errorf("commit fixture repo: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir bare fixture parent: %w", err)
	}
	if err := e2eGit(ctx, "", "clone", "--bare", workDir, barePath); err != nil {
		return "", fmt.Errorf("clone bare fixture repo: %w", err)
	}
	return barePath, nil
}

func gitLabReadOnlyRepoRef(cloneURL string) platform.RepoRef {
	return platform.RepoRef{
		Platform:      platform.KindGitLab,
		Host:          "gitlab.example.com",
		Owner:         "group",
		Name:          "project",
		RepoPath:      "group/project",
		WebURL:        "https://gitlab.example.com/group/project",
		CloneURL:      cloneURL,
		DefaultBranch: "main",
	}
}

func activityIdentityRepoRef(repo db.Repo, configuredRepoPath string) ghclient.RepoRef {
	return ghclient.RepoRef{
		Platform:           platform.Kind(repo.Platform),
		RepoID:             repo.ID,
		Owner:              repo.Owner,
		Name:               repo.Name,
		PlatformHost:       repo.PlatformHost,
		RepoPath:           repo.RepoPath,
		PlatformExternalID: repo.PlatformRepoID,
		WebURL:             repo.WebURL,
		CloneURL:           repo.CloneURL,
		DefaultBranch:      repo.DefaultBranch,
		ConfiguredRepoPath: configuredRepoPath,
	}
}

func gitLabReadOnlyIssueFixture(
	now time.Time,
	cloneURL string,
) (platform.Issue, []platform.IssueEvent) {
	ref := gitLabReadOnlyRepoRef(cloneURL)
	issue := platform.Issue{
		Repo:         ref,
		PlatformID:   7101,
		Number:       11,
		URL:          "https://gitlab.example.com/group/project/-/issues/11",
		Title:        "GitLab read-only issue",
		Author:       "ada",
		State:        "open",
		Body:         "GitLab issue body",
		CommentCount: 1,
		CreatedAt:    now.Add(-48 * time.Hour),
		UpdatedAt:    now,
	}
	events := []platform.IssueEvent{
		{
			Repo:        ref,
			PlatformID:  7201,
			IssueNumber: 11,
			EventType:   "issue_comment",
			Author:      "ada",
			Body:        "GitLab read-only timeline comment",
			CreatedAt:   now,
			DedupeKey:   "gitlab-read-only-issue-comment",
		},
	}
	return issue, events
}

func seedLabelEditingFixture(
	ctx context.Context,
	database *db.DB,
	fc *testutil.FixtureClient,
) error {
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widgets"),
	)
	if err != nil {
		return fmt.Errorf("get widgets repo: %w", err)
	}
	if repo == nil {
		return nil
	}
	now := time.Now().UTC().Add(-time.Hour)
	catalog := []db.Label{
		{Name: "bug", Description: "Something is broken", Color: "d73a4a", IsDefault: true, UpdatedAt: now},
		{Name: "triage", Description: "Needs maintainer review", Color: "fbca04", UpdatedAt: now},
		{Name: "docs", Description: "Documentation", Color: "0075ca", UpdatedAt: now},
	}
	if err := database.ReplaceRepoLabelCatalog(ctx, repo.ID, catalog, now); err != nil {
		return fmt.Errorf("seed label catalog: %w", err)
	}
	if pr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1); err != nil {
		return fmt.Errorf("get seeded pr: %w", err)
	} else if pr != nil {
		if err := database.ReplaceMergeRequestLabels(ctx, repo.ID, pr.ID, catalog[:1]); err != nil {
			return fmt.Errorf("seed pr labels: %w", err)
		}
	}
	if issue, err := database.GetIssueByRepoIDAndNumber(ctx, repo.ID, 10); err != nil {
		return fmt.Errorf("get seeded issue: %w", err)
	} else if issue != nil {
		if err := database.ReplaceIssueLabels(ctx, repo.ID, issue.ID, catalog[:1]); err != nil {
			return fmt.Errorf("seed issue labels: %w", err)
		}
	}
	seedFixtureClientLabels(fc)
	return nil
}

func seedFixtureClientLabels(fc *testutil.FixtureClient) {
	if fc == nil {
		return
	}
	bug := &gh.Label{
		ID:          new(int64(1)),
		NodeID:      new("LABEL_bug"),
		Name:        new("bug"),
		Description: new("Something is broken"),
		Color:       new("d73a4a"),
		Default:     new(true),
	}
	docs := &gh.Label{ID: new(int64(2)), NodeID: new("LABEL_docs"), Name: new("docs"), Description: new("Documentation"), Color: new("0075ca")}
	triage := &gh.Label{ID: new(int64(3)), NodeID: new("LABEL_triage"), Name: new("triage"), Description: new("Needs maintainer review"), Color: new("fbca04")}
	if fc.Labels == nil {
		fc.Labels = make(map[string][]*gh.Label)
	}
	fc.Labels["acme/widgets"] = []*gh.Label{bug, docs, triage}
	for _, prs := range [][]*gh.PullRequest{
		fc.OpenPRs["acme/widgets"],
		fc.PRs["acme/widgets"],
	} {
		for _, pr := range prs {
			if pr.GetNumber() == 1 {
				pr.Labels = []*gh.Label{bug}
			}
		}
	}
	for _, issues := range [][]*gh.Issue{
		fc.OpenIssues["acme/widgets"],
		fc.Issues["acme/widgets"],
	} {
		for _, issue := range issues {
			if issue.GetNumber() == 10 {
				issue.Labels = []*gh.Label{bug}
			}
		}
	}
}

func seedReviewSuggestionFixture(
	ctx context.Context,
	database *db.DB,
	owner, name string,
	number int,
) error {
	repo, err := database.GetRepoByIdentity(
		ctx,
		db.GitHubRepoIdentity("github.com", owner, name),
	)
	if err != nil {
		return fmt.Errorf("get review suggestion repo: %w", err)
	}
	if repo == nil {
		return fmt.Errorf("get review suggestion repo: not found")
	}
	mr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, number)
	if err != nil {
		return fmt.Errorf("get review suggestion pull request: %w", err)
	}
	if mr == nil {
		return fmt.Errorf("get review suggestion pull request: not found")
	}

	const providerThreadID = "e2e-review-suggestion-1"
	now := time.Date(2026, 7, 1, 16, 30, 0, 0, time.UTC)
	body := "Consider returning the published value.\n\n```suggestion\nreturn publish();\n```"
	if err := database.UpsertMREvents(ctx, []db.MREvent{{
		MergeRequestID:     mr.ID,
		PlatformExternalID: providerThreadID,
		EventType:          "review_comment",
		Author:             "reviewer",
		Body:               body,
		CreatedAt:          now,
		DedupeKey:          "review-comment-" + providerThreadID,
	}}); err != nil {
		return fmt.Errorf("seed review suggestion event: %w", err)
	}
	line := 1
	if err := database.UpsertMRReviewThreads(ctx, mr.ID, []db.MRReviewThread{{
		ProviderThreadID:  providerThreadID,
		ProviderCommentID: providerThreadID,
		Body:              body,
		AuthorLogin:       "reviewer",
		Range: db.ReviewLineRange{
			Path:        "internal/cache.go",
			Side:        "right",
			Line:        line,
			NewLine:     &line,
			LineType:    "context",
			DiffHeadSHA: mr.PlatformHeadSHA,
			CommitSHA:   mr.PlatformHeadSHA,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}}); err != nil {
		return fmt.Errorf("seed review suggestion thread: %w", err)
	}
	return nil
}

func seedReviewSuggestionProviderFixture(
	fc *testutil.FixtureClient,
	owner, name string,
	number int,
	headSHA string,
) {
	const providerThreadID = "e2e-review-suggestion-1"
	now := time.Date(2026, 7, 1, 16, 30, 0, 0, time.UTC)
	body := "Consider returning the published value.\n\n```suggestion\nreturn publish();\n```"
	key := fmt.Sprintf("%s/%s#%d", owner, name, number)
	fc.ReviewThreads[key] = append(fc.ReviewThreads[key], platformgithub.PullRequestReviewThread{
		NodeID: providerThreadID,
		Path:   "internal/cache.go",
		Side:   "RIGHT",
		Line:   1,
		Comments: []platformgithub.PullRequestReviewThreadComment{{
			NodeID:           providerThreadID,
			DatabaseID:       6901,
			ReviewDatabaseID: 5012,
			SubjectType:      "LINE",
			Body:             body,
			AuthorLogin:      "reviewer",
			Path:             "internal/cache.go",
			Line:             1,
			URL:              "https://github.com/acme/widgets/pull/1#discussion_r6901",
			CommitID:         headSHA,
			OriginalCommitID: headSHA,
			CreatedAt:        now,
			UpdatedAt:        now,
		}},
	})
}

// seedAssigneeReviewerFixture gives acme/widgets#1 a starting assignee
// and requested reviewer in both SQLite and the fixture provider so the
// Playwright suite can exercise the assignee/reviewer pickers.
func seedAssigneeReviewerFixture(
	ctx context.Context,
	database *db.DB,
	fc *testutil.FixtureClient,
) error {
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widgets"),
	)
	if err != nil {
		return fmt.Errorf("get widgets repo: %w", err)
	}
	if repo == nil {
		return nil
	}
	pr, err := database.GetMergeRequestByRepoIDAndNumber(ctx, repo.ID, 1)
	if err != nil {
		return fmt.Errorf("get seeded pr: %w", err)
	}
	if pr != nil {
		if err := database.UpdateMergeRequestAssignees(ctx, repo.ID, pr.ID, []string{"alice"}); err != nil {
			return fmt.Errorf("seed pr assignees: %w", err)
		}
		if err := database.UpdateMergeRequestReviewers(ctx, repo.ID, pr.ID, []string{"carol"}); err != nil {
			return fmt.Errorf("seed pr reviewers: %w", err)
		}
	}
	if fc == nil {
		return nil
	}
	for _, prs := range [][]*gh.PullRequest{
		fc.OpenPRs["acme/widgets"],
		fc.PRs["acme/widgets"],
	} {
		for _, fixturePR := range prs {
			if fixturePR.GetNumber() == 1 {
				fixturePR.Assignees = []*gh.User{{Login: new("alice")}}
				fixturePR.RequestedReviewers = []*gh.User{{Login: new("carol")}}
			}
		}
	}
	return nil
}

func seedGitLabReadOnlyCapabilityFixture(
	ctx context.Context,
	database *db.DB,
	cloneURL string,
) error {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	issue, events := gitLabReadOnlyIssueFixture(now, cloneURL)
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "gitlab.example.com",
		PlatformRepoID: "7001",
		Owner:          issue.Repo.Owner,
		Name:           issue.Repo.Name,
		RepoPath:       issue.Repo.RepoPath,
	})
	if err != nil {
		return fmt.Errorf("upsert gitlab repo: %w", err)
	}
	if err := database.UpdateRepoProviderMetadata(ctx, repoID, db.RepoProviderMetadata{
		PlatformRepoID: "7001",
		WebURL:         issue.Repo.WebURL,
		CloneURL:       issue.Repo.CloneURL,
		DefaultBranch:  issue.Repo.DefaultBranch,
	}); err != nil {
		return fmt.Errorf("update gitlab repo metadata: %w", err)
	}
	issueID, err := database.UpsertIssue(ctx, &db.Issue{
		RepoID:          repoID,
		PlatformID:      issue.PlatformID,
		Number:          issue.Number,
		URL:             issue.URL,
		Title:           issue.Title,
		Author:          issue.Author,
		State:           issue.State,
		Body:            issue.Body,
		CommentCount:    issue.CommentCount,
		CreatedAt:       issue.CreatedAt,
		UpdatedAt:       issue.UpdatedAt,
		LastActivityAt:  now,
		DetailFetchedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("upsert gitlab issue: %w", err)
	}
	commentID := events[0].PlatformID
	if err := database.UpsertIssueEvents(ctx, []db.IssueEvent{
		{
			IssueID:    issueID,
			PlatformID: &commentID,
			EventType:  events[0].EventType,
			Author:     events[0].Author,
			Body:       events[0].Body,
			CreatedAt:  events[0].CreatedAt,
			DedupeKey:  events[0].DedupeKey,
		},
	}); err != nil {
		return fmt.Errorf("upsert gitlab issue event: %w", err)
	}
	return nil
}

func giteaProviderCollisionIssue(now time.Time) platform.Issue {
	return platform.Issue{
		Repo: platform.RepoRef{
			Platform:           platform.KindGitea,
			Host:               "github.com",
			Owner:              "acme",
			Name:               "widgets",
			RepoPath:           "acme/widgets",
			PlatformID:         9100,
			PlatformExternalID: "gitea-acme-widgets",
			WebURL:             "https://github.com/acme/widgets",
			CloneURL:           "https://github.com/acme/widgets.git",
			DefaultBranch:      "main",
		},
		PlatformID:         9101,
		PlatformExternalID: "gitea-acme-widgets-901",
		Number:             901,
		URL:                "https://github.com/acme/widgets/issues/901",
		Title:              "Gitea provider collision issue",
		Author:             "gina",
		State:              "open",
		Body:               "Synthetic provider-collision issue for e2e filtering.",
		CommentCount:       0,
		CreatedAt:          now.Add(-2 * time.Hour),
		UpdatedAt:          now,
		LastActivityAt:     now,
	}
}

func seedGiteaProviderCollisionFixture(
	ctx context.Context,
	database *db.DB,
	issue platform.Issue,
) error {
	repoID, err := database.UpsertRepo(ctx, db.RepoIdentity{
		Platform:       string(issue.Repo.Platform),
		PlatformHost:   issue.Repo.Host,
		PlatformRepoID: issue.Repo.PlatformExternalID,
		Owner:          issue.Repo.Owner,
		Name:           issue.Repo.Name,
		RepoPath:       issue.Repo.RepoPath,
	})
	if err != nil {
		return fmt.Errorf("upsert gitea collision repo: %w", err)
	}
	if _, err := database.UpsertMergeRequest(ctx, &db.MergeRequest{
		RepoID:             repoID,
		PlatformID:         9102,
		PlatformExternalID: "gitea-acme-widgets-902",
		Number:             902,
		URL:                "https://github.com/acme/widgets/pulls/902",
		Title:              "Gitea provider collision pull request",
		Author:             "gina",
		AuthorDisplayName:  "Gina",
		State:              "open",
		HeadBranch:         "provider-collision",
		BaseBranch:         "main",
		CreatedAt:          issue.CreatedAt,
		UpdatedAt:          issue.UpdatedAt,
		LastActivityAt:     issue.LastActivityAt,
	}); err != nil {
		return fmt.Errorf("upsert gitea collision pull request: %w", err)
	}
	if _, err := database.UpsertIssue(ctx, &db.Issue{
		RepoID:             repoID,
		PlatformID:         issue.PlatformID,
		PlatformExternalID: issue.PlatformExternalID,
		Number:             issue.Number,
		URL:                issue.URL,
		Title:              issue.Title,
		Author:             issue.Author,
		State:              issue.State,
		Body:               issue.Body,
		CommentCount:       issue.CommentCount,
		CreatedAt:          issue.CreatedAt,
		UpdatedAt:          issue.UpdatedAt,
		LastActivityAt:     issue.LastActivityAt,
		DetailFetchedAt:    &issue.UpdatedAt,
	}); err != nil {
		return fmt.Errorf("upsert gitea collision issue: %w", err)
	}
	return nil
}

// ciFixtureOptions controls the per-fixture choices that the
// pr-ci-state/* endpoints feed into setPR1CIState. Centralising the
// options struct keeps the divergent fields visible and forces every
// fixture through the same anti-resync + provider-pin path.
type ciFixtureOptions struct {
	// statusName is the CIStatus column value ("failure", "success",
	// "pending", etc.).
	statusName string
	// checksJSON is the raw CIChecksJSON to seed. The empty string ""
	// writes an empty payload (the status-only fixture case). The
	// helper always writes this value to CIChecksJSON — there is no
	// "leave it alone" / "no-op" mode. If a transient state ever
	// needs to skip touching CIChecksJSON, add a new option flag
	// rather than overloading this field.
	checksJSON string
	// pinProviderTo optionally pins the fixture GitHub client's
	// check-run status/conclusion for PR #1 so a sync triggered by a
	// route transition can't overwrite the seeded payload. Nil means
	// don't touch the fixture provider.
	pinProviderTo *struct {
		Status     string
		Conclusion string
	}
	// providerCheckRuns replaces the fixture provider's check runs for
	// PR #1 when the test needs refreshes to preserve a multi-check payload.
	providerCheckRuns []*gh.CheckRun
	// providerCheckRunError makes the fixture provider fail check-run refreshes for
	// PR #1 when no provider-side representation exists for the seeded DB state.
	providerCheckRunError error
}

func ciChecksToCheckRuns(checks []db.CICheck) []*gh.CheckRun {
	runs := make([]*gh.CheckRun, 0, len(checks))
	for _, check := range checks {
		name := check.Name
		status := check.Status
		conclusion := check.Conclusion
		url := check.URL
		app := check.App
		runs = append(runs, &gh.CheckRun{
			Name:       &name,
			Status:     &status,
			Conclusion: &conclusion,
			HTMLURL:    &url,
			App:        &gh.App{Name: &app},
		})
	}
	return runs
}

// setPR1CIState centralises the boilerplate shared by every
// /__e2e/pr-ci-state/* endpoint: repo lookup, CIStatus + CIChecksJSON
// write, the anti-resync detail_fetched_at stamp, an optional fixture
// check-run pin, and the JSON response. New endpoints reduce to a few
// lines of payload-building plus a single call here; the helper is the
// only place every fixture path converges so no future endpoint can
// forget the anti-resync stamp or the provider pin.
func setPR1CIState(
	w http.ResponseWriter,
	r *http.Request,
	database *db.DB,
	fc *testutil.FixtureClient,
	label string,
	opts ciFixtureOptions,
) {
	repo, err := database.GetRepoByIdentity(
		r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
	)
	if err != nil || repo == nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}
	if err := database.UpdateMRCIStatus(
		r.Context(), repo.ID, 1, opts.statusName, opts.checksJSON,
	); err != nil {
		http.Error(w, "update "+label+" CI", http.StatusInternalServerError)
		return
	}
	// Explicit anti-resync guarantee — every fixture stamps
	// detail_fetched_at with ci_had_pending=false so the sync engine
	// treats the seeded row as fresh and doesn't refetch + overwrite
	// it. Centralised here so no future endpoint can forget it.
	if err := database.UpdateMRDetailFetchedByRepoID(
		r.Context(), repo.ID, 1, false,
	); err != nil {
		http.Error(w, "mark "+label+" CI fetched", http.StatusInternalServerError)
		return
	}
	if opts.pinProviderTo != nil {
		if !fc.SetPullRequestCheckRunStatus(
			"acme", "widgets", 1,
			opts.pinProviderTo.Status, opts.pinProviderTo.Conclusion,
		) {
			http.Error(w, "update fixture check runs", http.StatusNotFound)
			return
		}
	}
	if len(opts.providerCheckRuns) > 0 {
		if !fc.SetPullRequestCheckRuns("acme", "widgets", 1, opts.providerCheckRuns) {
			http.Error(w, "replace fixture check runs", http.StatusNotFound)
			return
		}
	}
	if opts.providerCheckRunError != nil {
		if !fc.SetPullRequestCheckRunError(
			"acme", "widgets", 1, opts.providerCheckRunError,
		) {
			http.Error(w, "set fixture check error", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": label}); err != nil {
		slog.Warn("write e2e response", "err", err)
	}
}

// appOptions parameterizes one in-process build of the e2e server
// state. The same options feed the initial startup and every
// /__e2e/reset rebuild.
type appOptions struct {
	roborevEndpoint      string
	defaultPlatformHost  string
	visibleImportedModes bool
	providerCollision    bool
	preferPtyOwner       bool
	nodeID               string
	federation           *e2eFederationRuntime
}

type e2eFederationRuntime struct {
	fleet       config.Fleet
	credentials *federationauth.Store
	enrollments *federation.Store
	httpClient  *http.Client
	activeSpoke bool
	localToken  string
	workspaceID string
	itemNumber  int
}

// appState bundles everything one logical e2e server instance owns:
// temp dir, database, fixture wiring, and the HTTP handler.
// /__e2e/reset swaps a fresh appState in and closes the old one so
// Playwright tests can reuse the process (and its port) instead of
// paying a full spawn/teardown per test.
type appState struct {
	tmpDir       string
	database     *db.DB
	srv          *server.Server
	handler      http.Handler
	cfgPath      string
	worktreeDir  string
	tmuxCommand  []string
	tmuxGate     *tmuxCreationGate
	ptyOwner     bool
	clones       *gitclone.Manager
	handlerWG    sync.WaitGroup
	tmuxStopOnce sync.Once
}

type appStateRegistry struct {
	mu      sync.Mutex
	current atomic.Pointer[appState]
	closers sync.WaitGroup
}

func newAppStateRegistry(initial *appState) *appStateRegistry {
	registry := &appStateRegistry{}
	registry.current.Store(initial)
	return registry
}

func (r *appStateRegistry) Load() *appState {
	return r.current.Load()
}

func (r *appStateRegistry) Swap(next *appState) *appState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.Swap(next)
}

func (r *appStateRegistry) closeAsync(close func()) {
	r.closers.Go(func() {
		close()
	})
}

func (r *appStateRegistry) waitForClosers() {
	r.closers.Wait()
}

func (r *appStateRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	state := r.startRequest()
	defer state.finishRequest()
	state.handler.ServeHTTP(w, req)
}

func (r *appStateRegistry) startRequest() *appState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.current.Load()
	// Swap takes the same lock, so any request that observes the old
	// state increments its handler count before old-state teardown can wait.
	state.handlerWG.Add(1)
	return state
}

func (st *appState) finishRequest() {
	st.handlerWG.Done()
}

func (st *appState) waitForHandlers(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		st.handlerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tmuxSocketCounter feeds per-instance tmux socket names so
// concurrent e2e server states never share a tmux server. Isolated
// sockets are what allow workspace/tmux tests to run in parallel
// instead of serializing behind a machine-wide lock. The random
// suffix guards against PID reuse attaching a later run to a stale
// tmux server left behind by a crashed process.
var tmuxSocketCounter atomic.Int64

func instanceTmuxCommand() []string {
	var randSuffix [4]byte
	if _, err := cryptorand.Read(randSuffix[:]); err != nil {
		// Extremely unlikely; pid+counter still keep concurrent
		// states apart, only crash+pid-reuse protection degrades.
		slog.Warn("tmux socket random suffix", "err", err)
	}
	name := fmt.Sprintf(
		"mm-e2e-%d-%d-%s",
		os.Getpid(),
		tmuxSocketCounter.Add(1),
		hex.EncodeToString(randSuffix[:]),
	)
	if root := strings.TrimSpace(os.Getenv(e2eTmuxDirEnv)); root != "" {
		return []string{
			"tmux", "-f", "/dev/null", "-S",
			filepath.Join(root, name+".sock"),
		}
	}
	return []string{"tmux", "-f", "/dev/null", "-L", name}
}

type tmuxCreationGate struct {
	dir string
}

func newTmuxCreationGate(root string, tmuxCmd []string) (*tmuxCreationGate, []string, error) {
	dir := filepath.Join(root, "tmux-gate")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create e2e tmux gate: %w", err)
	}
	script := filepath.Join(dir, "tmux")
	content := `#!/bin/sh
gate=$1
shift
case " $* " in
  *" new-session "*) ;;
  *) exec "$@" ;;
esac
if [ -e "$gate/killing" ]; then
  echo "e2e tmux shutdown has started" >&2
  exit 75
fi
while ! mkdir "$gate/creation.lock" 2>/dev/null; do
  if [ -e "$gate/killing" ]; then
    echo "e2e tmux shutdown has started" >&2
    exit 75
  fi
  sleep 0.01
done
trap 'rmdir "$gate/creation.lock"' EXIT HUP INT TERM
if [ -e "$gate/killing" ]; then
  echo "e2e tmux shutdown has started" >&2
  exit 75
fi
"$@"
`
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		return nil, nil, fmt.Errorf("write e2e tmux gate: %w", err)
	}
	command := make([]string, 0, 2+len(tmuxCmd))
	command = append(command, script, dir)
	command = append(command, tmuxCmd...)
	return &tmuxCreationGate{dir: dir}, command, nil
}

func (g *tmuxCreationGate) stop(kill func()) {
	if g == nil {
		kill()
		return
	}
	marker := filepath.Join(g.dir, "killing")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		slog.Warn("mark e2e tmux killing", "err", err)
	}
	lock := filepath.Join(g.dir, "creation.lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(lock, 0o700)
		if err == nil {
			defer os.Remove(lock)
			break
		}
		if !errors.Is(err, os.ErrExist) {
			slog.Warn("acquire e2e tmux creation gate", "err", err)
			break
		}
		if time.Now().After(deadline) {
			slog.Warn("wait for e2e tmux creation gate", "err", context.DeadlineExceeded)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	kill()
}

// killTmuxServer tears down the per-instance tmux server. It only
// acts on sockets named by instanceTmuxCommand so a misconfigured
// command can never kill a developer's real tmux server.
func killTmuxServer(tmuxCmd []string) {
	if !isOwnedE2ETmuxCommand(tmuxCmd) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(slices.Clone(tmuxCmd[1:]), "kill-server")
	_ = procutil.CommandContext(ctx, tmuxCmd[0], args...).Run()
}

func isOwnedE2ETmuxCommand(tmuxCmd []string) bool {
	for _, flag := range []string{"-L", "-S"} {
		idx := slices.Index(tmuxCmd, flag)
		if idx < 0 || idx+1 >= len(tmuxCmd) {
			continue
		}
		name := tmuxCmd[idx+1]
		if flag == "-S" {
			if !strings.HasPrefix(
				filepath.Base(filepath.Dir(name)),
				"kf-e2e-tmux-",
			) {
				return false
			}
			name = filepath.Base(name)
			if !strings.HasSuffix(name, ".sock") {
				return false
			}
		}
		return strings.HasPrefix(name, "mm-e2e-")
	}
	return false
}

func (st *appState) stopTmux() {
	st.tmuxStopOnce.Do(func() {
		st.tmuxGate.stop(func() {
			killTmuxServer(st.tmuxCommand)
		})
	})
}

// close releases everything the state owns. Shutdown drains HTTP
// handlers and background goroutines before the workspace cleanup
// and database close, mirroring the old process-exit defer ordering.
func (st *appState) close() {
	// tmux is an isolated test resource, not durable product state. Tear it
	// down before graceful HTTP draining so a stuck handler cannot strand the
	// daemon after SIGTERM or a reset.
	st.stopTmux()
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()
	if err := st.srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown", "err", err)
	}
	if err := st.waitForHandlers(shutdownCtx); err != nil {
		slog.Warn("drain e2e handlers", "err", err)
	}
	cleanupE2EWorkspaces(st.database, st.clones, st.worktreeDir, st.tmuxCommand, st.ptyOwner)
	if err := st.database.Close(); err != nil {
		slog.Warn("close database", "err", err)
	}
	if err := os.RemoveAll(st.tmpDir); err != nil {
		slog.Warn("remove e2e temp dir", "err", err)
	}
}

func seedFederatedE2EWorkspace(
	ctx context.Context,
	database *db.DB,
	tmpDir, workspaceID string,
	itemNumber int,
) error {
	repo, err := database.GetRepoByIdentity(
		ctx, db.GitHubRepoIdentity("github.com", "acme", "widgets"),
	)
	if err != nil {
		return fmt.Errorf("read federated fixture repository: %w", err)
	}
	if repo == nil {
		return errors.New("federated fixture repository is missing")
	}
	pull, err := database.GetMergeRequestByRepoIDAndNumber(
		ctx, repo.ID, itemNumber,
	)
	if err != nil {
		return fmt.Errorf("read federated fixture pull: %w", err)
	}
	if pull == nil {
		return fmt.Errorf("federated fixture pull %d is missing", itemNumber)
	}
	worktreePath := filepath.Join(tmpDir, "federated-workspaces", workspaceID)
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		return fmt.Errorf("create federated fixture workspace: %w", err)
	}
	return database.InsertWorkspace(ctx, &db.Workspace{
		ID: workspaceID, Platform: "github", PlatformHost: "github.com",
		RepoOwner: "acme", RepoName: "widgets",
		ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: itemNumber,
		ItemKey: strconv.Itoa(itemNumber), GitHeadRef: pull.HeadBranch,
		WorkspaceBranch: pull.HeadBranch, WorktreePath: worktreePath,
		Status: "ready", CreatedAt: time.Now().UTC(),
	})
}

// buildAppState seeds a complete e2e server state: fixture DB, git
// repos, config file, provider registry, and the HTTP handler with
// the /__e2e fixture endpoints. It runs at startup and on every
// /__e2e/reset.
func buildAppState(
	ctx context.Context,
	assets fs.FS,
	opts appOptions,
) (*appState, error) {
	defaultPlatformHost := strings.TrimSpace(opts.defaultPlatformHost)
	if defaultPlatformHost == "" {
		defaultPlatformHost = "github.com"
	}
	roborevEndpoint := opts.roborevEndpoint

	tmpDir, err := os.MkdirTemp("", "kenn-forge-e2e-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	built := false
	defer func() {
		if !built {
			os.RemoveAll(tmpDir)
		}
	}()

	database, err := db.Open(tmpDir + "/e2e.db")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if !built {
			database.Close()
		}
	}()

	result, err := testutil.SeedFixtures(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("seed fixtures: %w", err)
	}
	if opts.federation != nil && opts.federation.workspaceID != "" {
		if err := seedFederatedE2EWorkspace(
			ctx, database, tmpDir,
			opts.federation.workspaceID, opts.federation.itemNumber,
		); err != nil {
			return nil, err
		}
	}
	gitLabCloneURL, err := createBareRepoFixture(
		ctx,
		tmpDir,
		"gitlab.example.com",
		"group",
		"project",
	)
	if err != nil {
		return nil, fmt.Errorf("create gitlab fixture repo: %w", err)
	}
	if err := seedGitLabReadOnlyCapabilityFixture(ctx, database, gitLabCloneURL); err != nil {
		return nil, fmt.Errorf("seed gitlab capability fixture: %w", err)
	}

	// Run stack detection so seeded stacked chains are discoverable
	// via /api/v1/stacks and the PR detail sidebar.
	for _, rp := range []struct{ owner, name string }{
		{"acme", "widgets"},
		{"acme", "tools"},
	} {
		repo, err := database.GetRepoByIdentity(
			ctx, db.GitHubRepoIdentity("github.com", rp.owner, rp.name),
		)
		if err != nil || repo == nil {
			continue
		}
		if err := stacks.RunDetection(ctx, database, repo.ID); err != nil {
			return nil, fmt.Errorf("stack detection %s/%s: %w", rp.owner, rp.name, err)
		}
	}

	diffRepo, err := testutil.SetupDiffRepo(ctx, tmpDir, database)
	if err != nil {
		return nil, fmt.Errorf("setup diff repo: %w", err)
	}
	e2eWorktreeDir := filepath.Join(tmpDir, "worktrees")

	repos := []config.Repo{
		{Platform: "github", Owner: "acme", Name: "widgets"},
		{Platform: "github", Owner: "acme", Name: "tools"},
		{Platform: "github", Owner: "acme", Name: "archived"},
		{Platform: "github", Owner: "roborev-dev", Name: "*"},
	}
	if !strings.EqualFold(defaultPlatformHost, "github.com") {
		repos = []config.Repo{
			{
				Platform:     "github",
				Owner:        "enterprise",
				Name:         "service",
				PlatformHost: defaultPlatformHost,
			},
			{
				Platform:     "github",
				Owner:        "acme",
				Name:         "widgets",
				PlatformHost: "github.com",
			},
		}
	}
	if opts.providerCollision {
		repos = append(repos, config.Repo{
			Owner:        "acme",
			Name:         "widgets",
			Platform:     "gitea",
			PlatformHost: "github.com",
			RepoPath:     "acme/widgets",
		})
	}
	tmuxCommand := instanceTmuxCommand()
	if opts.preferPtyOwner {
		tmuxCommand = []string{filepath.Join(tmpDir, "missing-tmux")}
	}
	var tmuxGate *tmuxCreationGate
	guardedTmuxCommand := tmuxCommand
	if !opts.preferPtyOwner {
		tmuxGate, guardedTmuxCommand, err = newTmuxCreationGate(tmpDir, tmuxCommand)
		if err != nil {
			return nil, err
		}
	}
	cfg := &config.Config{
		SyncInterval:        "5m",
		GitHubTokenEnv:      "KENN_FORGE_GITHUB_TOKEN",
		DefaultPlatformHost: defaultPlatformHost,
		Host:                "127.0.0.1",
		Port:                8091,
		BasePath:            "/",
		Repos:               repos,
		Activity: config.Activity{
			ViewMode:  "flat",
			TimeRange: "7d",
		},
		// Private per-instance tmux socket so concurrent e2e states
		// (parallel Playwright workers, multiple worktrees) never
		// contend on one tmux server. This is what lets workspace
		// tests run unserialized.
		Tmux: config.Tmux{Command: guardedTmuxCommand},
	}
	if opts.federation != nil {
		cfg.Fleet = opts.federation.fleet
		cfg.API.RequireAuth = true
	}
	if opts.visibleImportedModes {
		modes := config.DefaultModeVisibility()
		*modes.Docs = true
		cfg.Modes = modes
	}

	cfg.Roborev.Endpoint = roborevEndpoint
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate e2e config: %w", err)
	}
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := cfg.Save(cfgPath); err != nil {
		return nil, fmt.Errorf("save e2e config: %w", err)
	}

	fc := result.FixtureClient()
	if err := seedLabelEditingFixture(ctx, database, fc); err != nil {
		return nil, fmt.Errorf("seed label editing fixture: %w", err)
	}
	if err := seedAssigneeReviewerFixture(ctx, database, fc); err != nil {
		return nil, fmt.Errorf("seed assignee reviewer fixture: %w", err)
	}
	fc.ListRepositoriesByOwnerFn = func(
		ctx context.Context, owner string,
	) ([]*gh.Repository, error) {
		pushedForge := gh.Timestamp{Time: time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)}
		pushedWorker := gh.Timestamp{Time: time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)}
		pushedBot := gh.Timestamp{Time: time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)}
		privateFalse := false
		if owner == "import-lab" {
			return []*gh.Repository{
				{
					NodeID:      new("repo-import-lab-api"),
					Name:        new("api"),
					Owner:       &gh.User{Login: new(owner)},
					Description: new("Import API"),
					Private:     &privateFalse,
					Archived:    new(false),
					PushedAt:    &pushedForge,
				},
				{
					NodeID:      new("repo-import-lab-worker"),
					Name:        new("worker"),
					Owner:       &gh.User{Login: new(owner)},
					Description: new("Import worker"),
					Private:     &privateFalse,
					Archived:    new(false),
					PushedAt:    &pushedWorker,
				},
				{
					NodeID:      new("repo-import-lab-archived"),
					Name:        new("archived"),
					Owner:       &gh.User{Login: new(owner)},
					Description: new("Archived import fixture"),
					Private:     &privateFalse,
					Archived:    new(true),
					PushedAt:    &pushedBot,
				},
			}, nil
		}
		if owner != "roborev-dev" {
			return fc.ReposByOwner[owner], nil
		}

		repos := []*gh.Repository{
			{
				NodeID:      new("repo-roborev-dev-kenn-forge"),
				Name:        new("kenn-forge"),
				Owner:       &gh.User{Login: new(owner)},
				Description: new("Main dashboard"),
				Private:     &privateFalse,
				Archived:    new(false),
				PushedAt:    &pushedForge,
			},
			{
				NodeID:      new("repo-roborev-dev-worker"),
				Name:        new("worker"),
				Owner:       &gh.User{Login: new(owner)},
				Description: new("Background jobs"),
				Private:     &privateFalse,
				Archived:    new(false),
				PushedAt:    &pushedWorker,
			},
			{
				NodeID:      new("repo-roborev-dev-archived"),
				Name:        new("archived"),
				Owner:       &gh.User{Login: new(owner)},
				Description: new("Archived service"),
				Private:     new(false),
				Archived:    new(true),
				PushedAt:    &pushedBot,
			},
		}
		if includeRefreshRepo, _ := ctx.Value(globRefreshContextKey{}).(bool); includeRefreshRepo {
			repos = append(repos, &gh.Repository{
				NodeID:      new("repo-roborev-dev-review-bot"),
				Name:        new("review-bot"),
				Owner:       &gh.User{Login: new(owner)},
				Description: new("Review automation"),
				Private:     &privateFalse,
				Archived:    new(false),
				PushedAt:    &pushedBot,
			})
		}
		return repos, nil
	}
	patchFixturePRSHAs(fc, "acme", "widgets", 1, diffRepo.HeadSHA, diffRepo.BaseSHA)
	seedReviewSuggestionProviderFixture(fc, "acme", "widgets", 1, diffRepo.HeadSHA)
	if err := seedReviewSuggestionFixture(ctx, database, "acme", "widgets", 1); err != nil {
		return nil, err
	}
	for _, target := range []struct {
		owner  string
		name   string
		number int
	}{
		{owner: "acme", name: "widgets", number: 7},
		{owner: "acme", name: "tools", number: 1},
	} {
		repo, err := database.GetRepoByIdentity(
			ctx, db.GitHubRepoIdentity("github.com", target.owner, target.name),
		)
		if err != nil {
			return nil, fmt.Errorf("get %s/%s repo: %w", target.owner, target.name, err)
		}
		if repo == nil {
			return nil, fmt.Errorf("get %s/%s repo: not found", target.owner, target.name)
		}
		headSHA := fc.PullRequestHeadSHA(target.owner, target.name, target.number)
		if headSHA == "" {
			return nil, fmt.Errorf(
				"get %s/%s#%d fixture head SHA: not found",
				target.owner, target.name, target.number,
			)
		}
		if err := database.UpdateDiffSHAs(
			ctx, repo.ID, target.number,
			headSHA, "", "",
		); err != nil {
			return nil, fmt.Errorf(
				"seed reviewed diff SHAs for %s/%s#%d: %w",
				target.owner, target.name, target.number, err,
			)
		}
	}

	fixtureClients := map[string]ghclient.Client{
		"github.com":        fc,
		defaultPlatformHost: fc,
	}
	startupResolved := ghclient.ResolveConfiguredRepos(
		ctx,
		fixtureClients,
		cfg.Repos,
	)
	for _, repo := range startupResolved.Expanded {
		if _, err := database.UpsertRepo(
			ctx, db.RepoIdentity{
				Platform:       string(repo.Platform),
				PlatformHost:   repo.PlatformHost,
				PlatformRepoID: repo.PlatformExternalID,
				Owner:          repo.Owner,
				Name:           repo.Name,
				RepoPath:       repo.RepoPath,
			},
		); err != nil {
			return nil, fmt.Errorf("seed startup repo %s/%s: %w", repo.Owner, repo.Name, err)
		}
	}
	if !strings.EqualFold(defaultPlatformHost, "github.com") {
		if _, err := database.UpsertRepo(
			ctx, db.RepoIdentity{
				Platform:       "github",
				PlatformHost:   defaultPlatformHost,
				PlatformRepoID: "e2e-enterprise-service",
				Owner:          "enterprise",
				Name:           "service",
				RepoPath:       "enterprise/service",
			},
		); err != nil {
			return nil, fmt.Errorf("seed default-host repo: %w", err)
		}
	}

	rt := ghclient.NewRateTracker(database, "github.com", "host", "rest")
	// Seed with known values so the budget bars render.
	rt.UpdateFromRate(ghclient.Rate{
		Limit:     5000,
		Remaining: 4200,
		Reset:     time.Now().Add(45 * time.Minute),
	})

	gqlRT := ghclient.NewRateTracker(database, "github.com", "host", "graphql")
	gqlRT.UpdateFromRate(ghclient.Rate{
		Limit:     5000,
		Remaining: 4800,
		Reset:     time.Now().Add(40 * time.Minute),
	})

	budget := ghclient.NewSyncBudgetWithEssentialReserve(500)
	budget.Spend(75)

	gitLabIssue, gitLabIssueEvents := gitLabReadOnlyIssueFixture(
		time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		gitLabCloneURL,
	)
	giteaCollisionIssue := giteaProviderCollisionIssue(
		time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
	)
	forgeUpdated := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	giteaUpdated := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	staticProviders := []platform.Provider{
		e2eStaticProvider{
			kind:        platform.KindGitLab,
			host:        "gitlab.example.com",
			issue:       gitLabIssue,
			issueEvents: gitLabIssueEvents,
			caps: platform.Capabilities{
				ReadIssues:            true,
				ReadComments:          true,
				ReadAuthenticatedUser: true,
			},
		},
	}
	if opts.providerCollision {
		staticProviders = append(staticProviders, e2eStaticProvider{
			kind:  platform.KindGitea,
			host:  "github.com",
			issue: giteaCollisionIssue,
			caps: platform.Capabilities{
				ReadRepositories:      true,
				ReadIssues:            true,
				ReadAuthenticatedUser: true,
				AssigneeMutation:      true,
			},
			repos: []platform.Repository{{
				Ref: platform.RepoRef{
					Platform: platform.KindGitea,
					Host:     "github.com",
					Owner:    "acme",
					Name:     "widgets",
					RepoPath: "acme/widgets",
				},
				PlatformID:    9100,
				Description:   "Gitea provider collision repo",
				Private:       false,
				UpdatedAt:     giteaCollisionIssue.UpdatedAt,
				DefaultBranch: "main",
				WebURL:        "https://github.com/acme/widgets",
				CloneURL:      "https://github.com/acme/widgets.git",
			}},
		})
		if err := seedGiteaProviderCollisionFixture(ctx, database, giteaCollisionIssue); err != nil {
			return nil, fmt.Errorf("seed gitea provider collision fixture: %w", err)
		}
	}
	registry, err := ghclient.NewProviderRegistry(
		fixtureClients,
		append(staticProviders,
			e2eStaticProvider{
				kind: platform.KindForgejo,
				host: "codeberg.org",
				caps: platform.Capabilities{
					ReadRepositories: true,
				},
				repos: []platform.Repository{
					{
						Ref: platform.RepoRef{
							Platform: platform.KindForgejo,
							Host:     "codeberg.org",
							Owner:    "forge-lab",
							Name:     "service",
							RepoPath: "forge-lab/service",
						},
						PlatformExternalID: "forgejo-repo-service",
						Description:        "Forgejo service",
						Private:            false,
						UpdatedAt:          forgeUpdated,
						DefaultBranch:      "main",
						WebURL:             "https://codeberg.org/forge-lab/service",
						CloneURL:           "https://codeberg.org/forge-lab/service.git",
					},
					{
						Ref: platform.RepoRef{
							Platform: platform.KindForgejo,
							Host:     "codeberg.org",
							Owner:    "forge-lab",
							Name:     "archived",
							RepoPath: "forge-lab/archived",
						},
						PlatformExternalID: "forgejo-repo-archived",
						Archived:           true,
					},
				},
			},
			e2eStaticProvider{
				kind: platform.KindGitea,
				host: "gitea.com",
				caps: platform.Capabilities{
					ReadRepositories: true,
				},
				repos: []platform.Repository{
					{
						Ref: platform.RepoRef{
							Platform: platform.KindGitea,
							Host:     "gitea.com",
							Owner:    "gitea-team",
							Name:     "service",
							RepoPath: "gitea-team/service",
						},
						PlatformExternalID: "gitea-repo-service",
						Description:        "Gitea service",
						Private:            false,
						UpdatedAt:          giteaUpdated,
						DefaultBranch:      "main",
						WebURL:             "https://gitea.com/gitea-team/service",
						CloneURL:           "https://gitea.com/gitea-team/service.git",
					},
					{
						Ref: platform.RepoRef{
							Platform: platform.KindGitea,
							Host:     "gitea.com",
							Owner:    "gitea-team",
							Name:     "private-service",
							RepoPath: "gitea-team/private-service",
						},
						PlatformExternalID: "gitea-repo-private-service",
						Description:        "Private Gitea service",
						Private:            true,
						UpdatedAt:          giteaUpdated.Add(-time.Hour),
					},
					{
						Ref: platform.RepoRef{
							Platform: platform.KindGitea,
							Host:     "gitea.com",
							Owner:    "gitea-team",
							Name:     "archived",
							RepoPath: "gitea-team/archived",
						},
						PlatformExternalID: "gitea-repo-archived",
						Archived:           true,
					},
				},
			},
		)...,
	)
	if err != nil {
		return nil, fmt.Errorf("create e2e provider registry: %w", err)
	}
	trackedRepos := append(
		slices.Clone(startupResolved.Expanded),
		ghclient.RepoRef{
			Platform:      platform.KindGitLab,
			PlatformHost:  gitLabIssue.Repo.Host,
			Owner:         gitLabIssue.Repo.Owner,
			Name:          gitLabIssue.Repo.Name,
			RepoPath:      gitLabIssue.Repo.RepoPath,
			WebURL:        gitLabIssue.Repo.WebURL,
			CloneURL:      gitLabIssue.Repo.CloneURL,
			DefaultBranch: gitLabIssue.Repo.DefaultBranch,
		},
	)
	if opts.providerCollision {
		trackedRepos = append(trackedRepos, ghclient.RepoRef{
			Platform:      platform.KindGitea,
			PlatformHost:  "github.com",
			Owner:         "acme",
			Name:          "widgets",
			RepoPath:      "acme/widgets",
			WebURL:        "https://github.com/acme/widgets",
			CloneURL:      "https://github.com/acme/widgets.git",
			DefaultBranch: "main",
		})
	}
	syncer := ghclient.NewSyncerWithRegistry(
		registry,
		database, diffRepo.Manager, trackedRepos, time.Hour,
		map[string]*ghclient.RateTracker{
			"github.com":        rt,
			defaultPlatformHost: rt,
		},
		map[string]*ghclient.SyncBudget{
			"github.com":        budget,
			defaultPlatformHost: budget,
		},
	)

	// Wire GraphQL fetcher so GQL rate data appears in the endpoint.
	gqlFetcher := ghclient.NewGraphQLFetcher(
		staticTokenSource("fake-token"), "github.com", gqlRT, budget,
	)
	syncer.SetFetchers(map[string]*ghclient.GraphQLFetcher{
		"github.com":        gqlFetcher,
		defaultPlatformHost: gqlFetcher,
	})
	syncer.SetWatchInterval(cfg.ActivePRRefreshDuration())
	syncer.SetActiveMRWindow(cfg.ActivePRWindowDuration())

	serverSyncer := syncer
	serverOptions := server.ServerOptions{
		Clones:                        diffRepo.Manager,
		WorktreeDir:                   e2eWorktreeDir,
		HostCheckAllowLoopbackAnyPort: true,
		PtyOwnerInProcess:             opts.preferPtyOwner,
		FederationSpokeID:             opts.nodeID,
	}
	if opts.federation != nil {
		serverOptions.DaemonAccess = server.DaemonAccessOptions{
			Token: opts.federation.localToken, RequireAPIAuth: true,
		}
		serverOptions.FederationCredentials = opts.federation.credentials
		serverOptions.FederationEnrollments = opts.federation.enrollments
		serverOptions.FederationHTTPClient = opts.federation.httpClient
		serverOptions.FederationSpokeActive = opts.federation.activeSpoke
		serverOptions.DisableWorkspaceBackgroundMonitors = true
		if opts.federation.fleet.RoleOrDefault() == config.FleetRoleSpoke {
			serverSyncer = nil
		}
	}
	srv := server.NewWithConfig(
		database, serverSyncer, diffRepo.Manager, assets, cfg, cfgPath,
		serverOptions,
	)
	// Mirror production wiring so notification syncs nudge an open activity
	// feed to reload (the feed's incremental poll skips backfilled rows).
	syncer.SetOnNotificationSyncComplete(func() {
		srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
	})
	syncer.SetOnWatchedMRSyncCompleted(func() {
		srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
	})
	var failNextRepoBrowserTree atomic.Bool
	var failNextNotificationRead atomic.Bool
	forkGitRoot := filepath.Join(tmpDir, "forks")
	forkGitHandler := http.FileServer(http.Dir(forkGitRoot))
	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			strings.EqualFold(r.Host, "github.com") &&
			strings.HasPrefix(r.URL.Path, "/forker/widgets.git/") {
			forkGitHandler.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/issue-workspace/reused-branch" {
			identityClonePath, err := diffRepo.Manager.ClonePathForContext(
				gitclone.WithRepositoryIdentity(r.Context(), diffRepo.PlatformRepoID),
				"github", "github.com", "acme", "widgets",
			)
			if err != nil {
				http.Error(w, "resolve fixture clone", http.StatusInternalServerError)
				return
			}
			workspaceClonePath, err := diffRepo.Manager.ClonePath(
				"github", "github.com", "acme", "widgets",
			)
			if err != nil {
				http.Error(w, "resolve workspace fixture clone", http.StatusInternalServerError)
				return
			}
			const branch = "kenn-forge/issue-10-widget-rendering-broken-on-safari"
			for _, clonePath := range []string{identityClonePath, workspaceClonePath} {
				_, stderr, err := gitcmd.New().Run(
					r.Context(), clonePath, nil,
					"update-ref", "refs/heads/"+branch, diffRepo.BaseSHA,
				)
				if err != nil {
					http.Error(
						w,
						"create reused issue branch: "+string(stderr),
						http.StatusInternalServerError,
					)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-head-repo/fork" {
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", 1,
			)
			if err != nil {
				http.Error(w, "read pull request", http.StatusInternalServerError)
				return
			}
			if mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}
			forkSnapshot := *mr
			forkSnapshot.UpdatedAt = time.Now().UTC()
			clonePath, err := diffRepo.Manager.ClonePathForContext(
				gitclone.WithRepositoryIdentity(r.Context(), diffRepo.PlatformRepoID),
				"github", "github.com", "acme", "widgets",
			)
			if err != nil {
				http.Error(w, "resolve fixture clone", http.StatusInternalServerError)
				return
			}
			originOutput, err := gitcmd.New().Output(
				r.Context(), clonePath,
				"config", "--get", "remote.origin.url",
			)
			if err != nil {
				http.Error(w, "resolve fixture origin", http.StatusInternalServerError)
				return
			}
			originPath := strings.TrimSpace(string(originOutput))
			forkPath := filepath.Join(forkGitRoot, "forker", "widgets.git")
			if err := os.MkdirAll(filepath.Dir(forkPath), 0o755); err != nil {
				http.Error(w, "create fixture fork parent", http.StatusInternalServerError)
				return
			}
			if err := e2eGit(
				r.Context(), "", "clone", "--bare", originPath, forkPath,
			); err != nil {
				http.Error(
					w,
					"create fixture fork: "+err.Error(),
					http.StatusInternalServerError,
				)
				return
			}
			if err := e2eGit(r.Context(), forkPath, "update-server-info"); err != nil {
				http.Error(w, "prepare fixture fork: "+err.Error(), http.StatusInternalServerError)
				return
			}
			proxyURL := "http://" + r.Host
			for key, value := range map[string]string{
				"HTTP_PROXY": proxyURL, "http_proxy": proxyURL,
				"NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1",
			} {
				if err := os.Setenv(key, value); err != nil {
					http.Error(w, "configure fixture git proxy", http.StatusInternalServerError)
					return
				}
			}
			// This dedicated offline fixture intentionally serves the exact
			// github.com fork URL through its process-local HTTP proxy.
			diffRepo.Manager.SetAllowInsecureHTTP("github", "github.com", true)
			forkSnapshot.HeadRepoCloneURL = "http://github.com/forker/widgets.git"
			_, accepted, err := database.UpsertMergeRequestSnapshot(
				r.Context(), &forkSnapshot,
			)
			if err != nil {
				http.Error(w, "persist fork snapshot", http.StatusInternalServerError)
				return
			}
			if !accepted {
				http.Error(w, "fork snapshot rejected", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		const persistedRuntimePrefix = "/__e2e/workspaces/"
		const persistedRuntimeSuffix = "/persisted-runtime-sessions"
		if r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, persistedRuntimePrefix) &&
			strings.HasSuffix(r.URL.Path, persistedRuntimeSuffix) {
			workspaceID := strings.TrimSuffix(
				strings.TrimPrefix(r.URL.Path, persistedRuntimePrefix),
				persistedRuntimeSuffix,
			)
			if workspaceID == "" || strings.Contains(workspaceID, "/") {
				http.Error(w, "workspace id required", http.StatusBadRequest)
				return
			}
			sessions, err := database.ListWorkspaceRuntimeSessions(
				r.Context(), workspaceID,
			)
			if err != nil {
				http.Error(
					w,
					"read persisted runtime sessions",
					http.StatusInternalServerError,
				)
				return
			}
			targetKeys := make([]string, 0, len(sessions))
			for _, session := range sessions {
				targetKeys = append(targetKeys, session.TargetKey)
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"target_keys": targetKeys,
			}); err != nil {
				slog.Warn(
					"write persisted runtime session fixture response",
					"err", err,
				)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/stage-older-detail-event" {
			itemType := r.URL.Query().Get("item_type")
			number := 1
			body := "Pull request detail activity older than the feed cursor"
			commitSuffix := "1"
			parentID := int64(0)
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", number,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}
			parentID = mr.ID
			if itemType == "issue" {
				number = 10
				body = "Issue detail activity older than the feed cursor"
				commitSuffix = "2"
				issue, issueErr := database.GetIssue(
					r.Context(), "github", "github.com", "acme", "widgets", number,
				)
				if issueErr != nil || issue == nil {
					http.Error(w, "issue not found", http.StatusNotFound)
					return
				}
				parentID = issue.ID
			} else if itemType != "pr" {
				http.Error(w, "item_type must be pr or issue", http.StatusBadRequest)
				return
			}

			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repository not found", http.StatusNotFound)
				return
			}
			now := time.Now().UTC()
			if r.URL.Query().Get("hold_sync") == "true" {
				fc.HoldIssueComments()
			}
			fc.SeedIssueComment(
				"acme", "widgets", number, body, now.Add(-3*time.Minute),
			)
			if err := database.UpsertBranchCommits(r.Context(), []db.BranchCommit{{
				RepoID:         repo.ID,
				BranchName:     "main",
				CommitSHA:      strings.Repeat("a", 39) + commitSuffix,
				AuthorName:     "Fixture Maintainer",
				AuthorEmail:    "maintainer@example.invalid",
				AuthoredAt:     now.Add(-2 * time.Minute),
				CommitterName:  "Fixture Maintainer",
				CommitterEmail: "maintainer@example.invalid",
				CommittedAt:    now.Add(-time.Minute),
				Subject:        "Activity cursor leader",
			}}); err != nil {
				http.Error(w, "persist leading activity", http.StatusInternalServerError)
				return
			}
			var staleErr error
			if itemType == "pr" {
				_, staleErr = database.WriteDB().ExecContext(r.Context(), `
					UPDATE forge_merge_requests SET detail_fetched_at = ? WHERE id = ?`,
					now.Add(-time.Hour), parentID,
				)
			} else {
				_, staleErr = database.WriteDB().ExecContext(r.Context(), `
					UPDATE forge_issues SET detail_fetched_at = ? WHERE id = ?`,
					now.Add(-time.Hour), parentID,
				)
			}
			if staleErr != nil {
				http.Error(w, "make detail stale", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/release-older-detail-event-sync" {
			fc.ReleaseIssueComments()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/activity/item-comment" {
			if r.URL.Query().Get("require_subscriber") != "false" && srv.SubscriberCount() == 0 {
				http.Error(w, "event stream not connected", http.StatusConflict)
				return
			}
			itemType := r.URL.Query().Get("item_type")
			number := 1
			if itemType == "issue" {
				number = 10
			} else if itemType != "pr" {
				http.Error(w, "item_type must be pr or issue", http.StatusBadRequest)
				return
			}
			body := r.URL.Query().Get("body")
			if body == "" {
				body = "Persisted live Activity comment"
			}
			comment, err := fc.CreateIssueComment(
				r.Context(), "acme", "widgets", number, body,
			)
			if err != nil {
				http.Error(w, "create fixture item comment", http.StatusInternalServerError)
				return
			}
			if itemType == "pr" {
				mr, err := database.GetMergeRequest(
					r.Context(), "github", "github.com", "acme", "widgets", number,
				)
				if err != nil || mr == nil {
					http.Error(w, "pull request not found", http.StatusNotFound)
					return
				}
				if err := database.UpsertMREvents(r.Context(), []db.MREvent{
					ghclient.NormalizeCommentEvent(mr.ID, comment),
				}); err != nil {
					http.Error(w, "persist pull request event", http.StatusInternalServerError)
					return
				}
			} else {
				issue, err := database.GetIssue(
					r.Context(), "github", "github.com", "acme", "widgets", number,
				)
				if err != nil || issue == nil {
					http.Error(w, "issue not found", http.StatusNotFound)
					return
				}
				if err := database.UpsertIssueEvents(r.Context(), []db.IssueEvent{
					ghclient.NormalizeIssueCommentEvent(issue.ID, comment),
				}); err != nil {
					http.Error(w, "persist issue event", http.StatusInternalServerError)
					return
				}
			}
			eventID := srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
			w.Header().Set("X-Kenn-E2E-Event-ID", strconv.FormatUint(eventID, 10))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/stage-filtered-parent-recency" {
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", 6,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}
			outsideRange := time.Now().UTC().Add(-45 * 24 * time.Hour).Truncate(time.Second)
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				UPDATE forge_merge_requests
				SET created_at = ?, updated_at = ?, last_activity_at = ?
				WHERE id = ?`, outsideRange, outsideRange, outsideRange, mr.ID); err != nil {
				http.Error(w, "age pull request activity", http.StatusInternalServerError)
				return
			}
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				UPDATE forge_mr_events SET created_at = ? WHERE merge_request_id = ?`,
				outsideRange, mr.ID,
			); err != nil {
				http.Error(w, "age pull request events", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/filtered-parent-recency" {
			if srv.SubscriberCount() == 0 {
				http.Error(w, "event stream not connected", http.StatusConflict)
				return
			}
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", 6,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}
			// The comment is the ledger event that defines Activity recency.
			// The provider's updated_at is bumped further ahead, the way GitHub
			// does after mergeability recomputes, and must not win.
			activityAt := time.Now().UTC().Truncate(time.Second)
			comment := fc.SeedIssueComment("acme", "widgets", 6, "Filtered parent comment", activityAt)
			if err := database.UpsertMREvents(r.Context(), []db.MREvent{
				ghclient.NormalizeCommentEvent(mr.ID, comment),
			}); err != nil {
				http.Error(w, "persist filtered pull request event", http.StatusInternalServerError)
				return
			}
			providerBumpAt := activityAt.Add(10 * time.Minute)
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				UPDATE forge_merge_requests
				SET updated_at = ?, last_activity_at = ?
				WHERE id = ?`, providerBumpAt, providerBumpAt, mr.ID); err != nil {
				http.Error(w, "advance pull request activity", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Kenn-E2E-Parent-Activity-At", activityAt.Format(time.RFC3339Nano))
			srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/activity/pr-comments/sync" {
			number, err := strconv.Atoi(r.URL.Query().Get("number"))
			if err != nil || number <= 0 {
				http.Error(w, "positive pull request number required", http.StatusBadRequest)
				return
			}
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", number,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}
			comments, err := fc.ListIssueComments(r.Context(), "acme", "widgets", number)
			if err != nil {
				http.Error(w, "read fixture pull request comments", http.StatusInternalServerError)
				return
			}
			events := make([]db.MREvent, 0, len(comments))
			for _, comment := range comments {
				events = append(events, ghclient.NormalizeCommentEvent(mr.ID, comment))
			}
			if err := database.ReplaceMRCommentEvents(r.Context(), mr.ID, events); err != nil {
				http.Error(w, "reconcile fixture pull request comments", http.StatusInternalServerError)
				return
			}
			eventID := srv.Hub().Broadcast(server.Event{Type: "data_changed", Data: struct{}{}})
			w.Header().Set("X-Kenn-E2E-Event-ID", strconv.FormatUint(eventID, 10))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/viewed-hot-fast-sync" {
			if srv.SubscriberCount() == 0 {
				http.Error(w, "event stream not connected", http.StatusConflict)
				return
			}
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", 1,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}

			now := time.Now().UTC()
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				UPDATE forge_merge_requests
				SET last_activity_at = ?, detail_fetched_at = ?
				WHERE id = ?`, now.Add(-5*time.Hour), now.Add(-3*time.Minute), mr.ID); err != nil {
				http.Error(w, "make pull request detail stale", http.StatusInternalServerError)
				return
			}
			if _, err := fc.CreateIssueComment(
				r.Context(), "acme", "widgets", 1,
				"Viewed hot fast-sync comment",
			); err != nil {
				http.Error(w, "create provider pull request comment", http.StatusInternalServerError)
				return
			}

			syncer.SyncWatchedMRs(r.Context())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/notification-fast-sync" {
			if srv.SubscriberCount() == 0 {
				http.Error(w, "event stream not connected", http.StatusConflict)
				return
			}
			mr, err := database.GetMergeRequest(
				r.Context(), "github", "github.com", "acme", "widgets", 1,
			)
			if err != nil || mr == nil {
				http.Error(w, "pull request not found", http.StatusNotFound)
				return
			}

			now := time.Now().UTC()
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				UPDATE forge_merge_requests
				SET last_activity_at = ?, detail_fetched_at = ?
				WHERE id = ?`,
				now.Add(-5*time.Hour), now.Add(-10*time.Minute), mr.ID,
			); err != nil {
				http.Error(w, "make pull request detail stale", http.StatusInternalServerError)
				return
			}
			if _, err := database.WriteDB().ExecContext(r.Context(), `
				DELETE FROM forge_hot_merge_requests
				WHERE merge_request_id = ?`, mr.ID); err != nil {
				http.Error(w, "clear pull request hot view", http.StatusInternalServerError)
				return
			}
			if _, err := fc.CreateIssueComment(
				r.Context(), "acme", "widgets", 1,
				"Notification-driven fast-sync comment",
			); err != nil {
				http.Error(w, "create provider pull request comment", http.StatusInternalServerError)
				return
			}
			number := 1
			repoID := mr.RepoID
			if err := database.UpsertNotifications(r.Context(), []db.Notification{{
				Platform:               "github",
				PlatformHost:           "github.com",
				PlatformNotificationID: "notif-activity-fast-sync-1",
				RepoID:                 &repoID,
				RepoOwner:              "acme",
				RepoName:               "widgets",
				SubjectType:            "PullRequest",
				SubjectTitle:           "Add widget caching layer",
				WebURL:                 "https://github.com/acme/widgets/pull/1",
				ItemNumber:             &number,
				ItemType:               "pr",
				ItemAuthor:             "alice",
				Reason:                 "comment",
				Unread:                 true,
				Participating:          true,
				SourceUpdatedAt:        now,
				SyncedAt:               now,
			}}); err != nil {
				http.Error(w, "persist pull request notification", http.StatusInternalServerError)
				return
			}

			syncer.SyncWatchedMRs(r.Context())
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/default-branch-commit" {
			repo, err := database.GetRepoByIdentity(
				r.Context(),
				db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repository not found", http.StatusNotFound)
				return
			}
			committedAt := time.Now().UTC().Add(-time.Minute)
			if err := database.UpsertBranchCommits(r.Context(), []db.BranchCommit{{
				RepoID:         repo.ID,
				BranchName:     "main",
				CommitSHA:      "1234567890abcdef1234567890abcdef12345678",
				AuthorName:     "Fixture Maintainer",
				AuthorEmail:    "maintainer@example.invalid",
				AuthoredAt:     committedAt.Add(-time.Minute),
				CommitterName:  "Fixture Maintainer",
				CommitterEmail: "maintainer@example.invalid",
				CommittedAt:    committedAt,
				Subject:        "Repository maintenance commit",
			}}); err != nil {
				http.Error(w, "persist default branch commit", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/activity/repository-identity" {
			const (
				originalRepoPath    = "acme/widgets"
				renamedRepoPath     = "acme/widgets-renamed"
				replacementProvider = "e2e-replacement-widgets"
			)
			observedAt := time.Now().UTC().Add(time.Minute)
			var entry *db.RepositoryCatalogEntry
			var err error
			switch r.URL.Query().Get("phase") {
			case "rename":
				original, getErr := database.GetRepoByIdentity(
					r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
				)
				if getErr != nil || original == nil {
					http.Error(w, "original repository not found", http.StatusNotFound)
					return
				}
				entry, _, err = database.ReconcileRepositoryObservation(r.Context(), db.RepoIdentity{
					Platform:       original.Platform,
					PlatformHost:   original.PlatformHost,
					PlatformRepoID: original.PlatformRepoID,
					Owner:          "acme",
					Name:           "widgets-renamed",
					RepoPath:       renamedRepoPath,
				}, observedAt)
				if err == nil && entry != nil {
					_, err = database.WriteDB().ExecContext(r.Context(), `
						UPDATE forge_merge_requests
						SET url = ?
						WHERE repo_id = ? AND number = 1`,
						"https://github.com/acme/widgets-renamed/pull/1", entry.Repository.ID,
					)
				}
			case "reuse":
				entry, _, err = database.ReconcileRepositoryObservation(r.Context(), db.RepoIdentity{
					Platform:       "github",
					PlatformHost:   "github.com",
					PlatformRepoID: replacementProvider,
					Owner:          "acme",
					Name:           "widgets",
					RepoPath:       originalRepoPath,
				}, observedAt.Add(time.Minute))
				if err == nil && entry != nil {
					now := time.Now().UTC().Truncate(time.Second)
					_, err = database.UpsertMergeRequest(r.Context(), &db.MergeRequest{
						RepoID:             entry.Repository.ID,
						PlatformID:         990001,
						PlatformExternalID: "e2e-replacement-pull",
						Number:             1,
						URL:                "https://github.com/acme/widgets/pull/1",
						Title:              "Replacement route pull request",
						Author:             "replacement-author",
						State:              db.MergeRequestStateOpen,
						CreatedAt:          now,
						UpdatedAt:          now,
						LastActivityAt:     now,
					})
				}
			default:
				http.Error(w, "phase must be rename or reuse", http.StatusBadRequest)
				return
			}
			if err != nil || entry == nil {
				http.Error(w, "reconcile repository identity", http.StatusInternalServerError)
				return
			}
			syncer.SetRepos([]ghclient.RepoRef{
				activityIdentityRepoRef(entry.Repository, originalRepoPath),
			})
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"platform_repo_id": entry.Repository.PlatformRepoID,
				"repo_path":        entry.Repository.RepoPath,
			}); err != nil {
				slog.Warn("write repository identity fixture response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/repo-browser/tree/fail-next" {
			failNextRepoBrowserTree.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet &&
			strings.HasSuffix(r.URL.Path, "/browser/tree") &&
			failNextRepoBrowserTree.CompareAndSwap(true, false) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(httpapi.NewProblem(http.StatusInternalServerError, httpapi.CodeInternalError, "tree failed", nil))
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/notifications/fail-next-read" {
			failNextNotificationRead.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/api/v1/notifications/read" &&
			failNextNotificationRead.CompareAndSwap(true, false) {
			var input struct {
				IDs []int64 `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.IDs) == 0 {
				http.Error(w, "notification ids required", http.StatusBadRequest)
				return
			}
			failed := make([]map[string]any, 0, len(input.IDs))
			for _, id := range input.IDs {
				failed = append(failed, map[string]any{
					"id": id, "error": "fixture notification read failure",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"succeeded": []int64{},
				"queued":    []int64{},
				"failed":    failed,
			}); err != nil {
				slog.Warn("write notification read failure fixture", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/review-suggestion/succeed" {
			fc.SetReviewSuggestionResult(&platform.AppliedReviewSuggestions{
				CommitSHA: diffRepo.AltHeadSHA,
			}, diffRepo.BaseSHA)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/merge/conflict/stale-head" {
			patchFixturePRSHAs(
				fc, "acme", "widgets", 1,
				diffRepo.AltHeadSHA, diffRepo.BaseSHA,
			)
			fc.SetMergePullRequestError(&platform.Error{
				Code:         platform.ErrCodeStaleState,
				Provider:     platform.KindGitHub,
				PlatformHost: "github.com",
				Err:          errors.New("head commit changed"),
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/merge/conflict/not-open" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			closedAt := time.Now().UTC()
			if err := database.UpdateMRState(
				r.Context(), repo.ID, 1, string(db.MergeRequestStateClosed), nil, &closedAt,
			); err != nil {
				http.Error(w, "update pull request state", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/merge/conflict/open" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			if err := database.UpdateMRState(
				r.Context(), repo.ID, 1, string(db.MergeRequestStateOpen), nil, nil,
			); err != nil {
				http.Error(w, "update pull request state", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/merge/fail" {
			fc.SetMergePullRequestError(errors.New("provider rejected merge"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/__e2e/merge/not-merged" {
			merged := false
			sha := ""
			message := "provider did not merge the pull request"
			fc.SetMergePullRequestResult(&gh.PullRequestMergeResult{
				Merged:  &merged,
				SHA:     &sha,
				Message: &message,
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-workflow-approval/required" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			pendingChecks, err := json.Marshal([]db.CICheck{{
				Name:       "build",
				Status:     "in_progress",
				Conclusion: "",
				URL:        "https://github.com/acme/widgets/actions/runs/1/job/1",
				App:        "GitHub Actions",
			}})
			if err != nil {
				http.Error(w, "marshal pending checks", http.StatusInternalServerError)
				return
			}
			if err := database.UpdateMRCIStatus(
				r.Context(), repo.ID, 1, "pending", string(pendingChecks),
			); err != nil {
				http.Error(w, "update pending CI", http.StatusInternalServerError)
				return
			}
			if err := database.UpdateMRDetailFetchedByRepoID(
				r.Context(), repo.ID, 1, true,
			); err != nil {
				http.Error(w, "mark pending CI fetched", http.StatusInternalServerError)
				return
			}

			headSHA := fc.PullRequestHeadSHA("acme", "widgets", 1)
			if headSHA == "" {
				http.Error(w, "PR head SHA not found", http.StatusNotFound)
				return
			}
			runID := int64(9001)
			event := "pull_request"
			number := 1
			fc.SetWorkflowRuns("acme", "widgets", headSHA, []*gh.WorkflowRun{{
				ID:           &runID,
				HeadSHA:      &headSHA,
				Event:        &event,
				PullRequests: []*gh.PullRequest{{Number: &number}},
			}})

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{
				"status": "required",
				"run_id": runID,
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/repo-settings/viewer-can-merge/deny" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			if err := database.UpdateRepoViewerCanMerge(r.Context(), repo.ID, false); err != nil {
				http.Error(w, "update repo viewer merge permission", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]bool{
				"ViewerCanMerge": false,
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/pending" {
			pendingPayload, err := json.Marshal([]db.CICheck{{
				Name:       "build",
				Status:     "in_progress",
				Conclusion: "",
				URL:        "https://github.com/acme/widgets/actions/runs/1/job/1",
				App:        "GitHub Actions",
			}})
			if err != nil {
				http.Error(w, "marshal pending checks", http.StatusInternalServerError)
				return
			}
			setPR1CIState(w, r, database, fc, "pending", ciFixtureOptions{
				statusName: "pending",
				checksJSON: string(pendingPayload),
				pinProviderTo: &struct {
					Status     string
					Conclusion string
				}{Status: "in_progress", Conclusion: ""},
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/fail-refresh" {
			if !fc.SetPullRequestCheckRunError(
				"acme", "widgets", 1, errors.New("fixture CI refresh failed"),
			) {
				http.Error(w, "set fixture check error", http.StatusNotFound)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"status": "fail-refresh",
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/success" {
			successPayload, err := json.Marshal([]db.CICheck{
				{
					Name:       "build",
					Status:     "completed",
					Conclusion: "success",
					URL:        "https://github.com/acme/widgets/actions/runs/1/job/1",
					App:        "GitHub Actions",
				},
				{
					Name:       "test",
					Status:     "completed",
					Conclusion: "success",
					App:        "GitHub Actions",
				},
			})
			if err != nil {
				http.Error(w, "marshal success checks", http.StatusInternalServerError)
				return
			}
			setPR1CIState(w, r, database, fc, "success", ciFixtureOptions{
				statusName: "success",
				checksJSON: string(successPayload),
				pinProviderTo: &struct {
					Status     string
					Conclusion string
				}{Status: "completed", Conclusion: "success"},
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/mixed" {
			mixedPayload, err := json.Marshal([]db.CICheck{
				{
					Name:       "build-darwin",
					Status:     "completed",
					Conclusion: "failure",
					URL:        "https://github.com/acme/widgets/actions/runs/1/job/1",
					App:        "GitHub Actions",
				},
				{
					Name:       "build-linux",
					Status:     "completed",
					Conclusion: "success",
					App:        "GitHub Actions",
				},
				{
					Name:       "test-linux",
					Status:     "completed",
					Conclusion: "success",
					App:        "GitHub Actions",
				},
				{
					Name:       "deploy-staging",
					Status:     "in_progress",
					Conclusion: "",
					App:        "GitHub Actions",
				},
				{
					Name:       "build-windows",
					Status:     "completed",
					Conclusion: "skipped",
					App:        "GitHub Actions",
				},
			})
			if err != nil {
				http.Error(w, "marshal mixed checks", http.StatusInternalServerError)
				return
			}
			setPR1CIState(w, r, database, fc, "mixed", ciFixtureOptions{
				statusName: "failure",
				checksJSON: string(mixedPayload),
				pinProviderTo: &struct {
					Status     string
					Conclusion string
				}{Status: "completed", Conclusion: "failure"},
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/malformed" {
			// No fixture-provider analogue for malformed JSON exists
			// — a real sync would replace the seeded text with a
			// valid array. Keep check refreshes failing so any
			// incidental forced refresh preserves the seeded payload.
			setPR1CIState(w, r, database, fc, "malformed", ciFixtureOptions{
				statusName:            "failure",
				checksJSON:            "{not json",
				providerCheckRunError: errors.New("fixture malformed CI refresh failed"),
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/status-only" {
			// CIStatus is set but CIChecksJSON stays empty — exercises
			// the transient sync state where the redesigned UI hides
			// the chip. pinProviderTo stays nil so the provider can
			// remain aligned with the absent payload.
			setPR1CIState(w, r, database, fc, "status-only", ciFixtureOptions{
				statusName: "success",
				checksJSON: "",
				// pinProviderTo intentionally nil
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/pending-status-only" {
			// CIStatus is pending while CIChecksJSON is still empty.
			// Keep the provider pinned to pending so the deferred-merge
			// endpoint can refresh granular checks before queueing.
			setPR1CIState(w, r, database, fc, "pending-status-only", ciFixtureOptions{
				statusName: "pending",
				checksJSON: "",
				pinProviderTo: &struct {
					Status     string
					Conclusion string
				}{Status: "in_progress", Conclusion: ""},
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-ci-state/dropdown-mixed" {
			// 21-check payload spanning every bucket so the dropdown
			// e2e can exercise the summary header, all five sections,
			// and the "Show N more passed" toggle (passed count of 12
			// exceeds the 8-row threshold).
			checks := []db.CICheck{
				{
					Name:       "build-darwin",
					Status:     "completed",
					Conclusion: "failure",
					App:        "GitHub Actions",
				},
			}
			for i := 1; i <= 5; i++ {
				checks = append(checks, db.CICheck{
					Name:       fmt.Sprintf("pending-%d", i),
					Status:     "completed",
					Conclusion: "",
					App:        "GitHub Actions",
				})
			}
			for i := 1; i <= 12; i++ {
				checks = append(checks, db.CICheck{
					Name:       fmt.Sprintf("passed-%d", i),
					Status:     "completed",
					Conclusion: "success",
					App:        "GitHub Actions",
				})
			}
			checks = append(checks,
				db.CICheck{
					Name:       "skip-1",
					Status:     "completed",
					Conclusion: "skipped",
					App:        "GitHub Actions",
				},
				db.CICheck{
					Name:       "skip-2",
					Status:     "completed",
					Conclusion: "skipped",
					App:        "GitHub Actions",
				},
				db.CICheck{
					Name:       "weird",
					Status:     "completed",
					Conclusion: "mysterious_state",
					App:        "GitHub Actions",
				},
			)
			dropdownPayload, err := json.Marshal(checks)
			if err != nil {
				http.Error(w, "marshal dropdown-mixed checks", http.StatusInternalServerError)
				return
			}
			setPR1CIState(w, r, database, fc, "dropdown-mixed", ciFixtureOptions{
				statusName:        "failure",
				checksJSON:        string(dropdownPayload),
				providerCheckRuns: ciChecksToCheckRuns(checks),
			})
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/notifications/add-synced" {
			number := 6
			fc.Notifications = append(fc.Notifications, ghclient.NotificationThread{
				ID:            "notif-tools-synced-6",
				RepoOwner:     "acme",
				RepoName:      "tools",
				SubjectType:   "Issue",
				SubjectTitle:  "Synced tools notification",
				WebURL:        "https://github.com/acme/tools/issues/6",
				ItemNumber:    &number,
				ItemType:      "issue",
				ItemAuthor:    "dave",
				Reason:        "mention",
				Unread:        true,
				Participating: true,
				UpdatedAt:     time.Now().UTC(),
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-diff-summary/advance-head" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			if err := database.UpdateDiffSHAs(
				r.Context(), repo.ID, 1,
				diffRepo.AltHeadSHA, diffRepo.BaseSHA, diffRepo.BaseSHA,
			); err != nil {
				http.Error(w, "update diff shas", http.StatusInternalServerError)
				return
			}
			if err := database.UpdatePlatformSHAs(
				r.Context(), repo.ID, 1,
				diffRepo.AltHeadSHA, diffRepo.BaseSHA,
			); err != nil {
				http.Error(w, "update platform shas", http.StatusInternalServerError)
				return
			}
			patchFixturePRSHAs(
				fc, "acme", "widgets", 1,
				diffRepo.AltHeadSHA, diffRepo.BaseSHA,
			)
			eventID := srv.Hub().Broadcast(server.Event{
				Type: "pr_detail_refreshed",
				Data: workspaceapi.PRDetailRefreshedPayload{
					Provider:     "github",
					PlatformHost: "github.com",
					RepoPath:     "acme/widgets",
					Owner:        "acme",
					Name:         "widgets",
					Number:       1,
					HeadSHA:      diffRepo.AltHeadSHA,
					SyncedAt:     time.Now().UTC().Format(time.RFC3339),
					Warnings:     []string{},
				},
			})
			w.Header().Set("X-Kenn-E2E-Event-ID", strconv.FormatUint(eventID, 10))
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"head_sha": diffRepo.AltHeadSHA,
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-diff-context/large-head" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			if err := database.UpdateDiffSHAs(
				r.Context(), repo.ID, 1,
				diffRepo.ContextHeadSHA, diffRepo.BaseSHA, diffRepo.BaseSHA,
			); err != nil {
				http.Error(w, "update diff shas", http.StatusInternalServerError)
				return
			}
			if err := database.UpdatePlatformSHAs(
				r.Context(), repo.ID, 1,
				diffRepo.ContextHeadSHA, diffRepo.BaseSHA,
			); err != nil {
				http.Error(w, "update platform shas", http.StatusInternalServerError)
				return
			}
			patchFixturePRSHAs(
				fc, "acme", "widgets", 1,
				diffRepo.ContextHeadSHA, diffRepo.BaseSHA,
			)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"head_sha": diffRepo.ContextHeadSHA,
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			r.URL.Path == "/__e2e/pr-review-thread-regroup/add-reply" {
			repo, err := database.GetRepoByIdentity(
				r.Context(), db.GitHubRepoIdentity("github.com", "acme", "widgets"),
			)
			if err != nil || repo == nil {
				http.Error(w, "repo not found", http.StatusNotFound)
				return
			}
			mr, err := database.GetMergeRequestByRepoIDAndNumber(r.Context(), repo.ID, 1)
			if err != nil {
				http.Error(w, "get merge request", http.StatusInternalServerError)
				return
			}
			if mr == nil {
				http.Error(w, "merge request not found", http.StatusNotFound)
				return
			}
			threadID := "PRRT_reply_regroup"
			platformID := int64(6014)
			createdAt := time.Now().UTC()
			event := db.MREvent{
				MergeRequestID:     mr.ID,
				PlatformID:         &platformID,
				PlatformExternalID: "6014",
				EventType:          "review_comment",
				Author:             "fixture-bot",
				Body:               "Regroup reply added during detail refresh.",
				CreatedAt:          createdAt,
				DedupeKey:          "review_comment:6014",
				DirectURL:          "https://github.com/acme/widgets/pull/1#discussion_r6014",
				ThreadID:           &threadID,
				Resolvable:         true,
			}
			if err := database.UpsertMREvents(r.Context(), []db.MREvent{event}); err != nil {
				http.Error(w, "upsert regroup reply", http.StatusInternalServerError)
				return
			}
			headSHA := mr.PlatformHeadSHA
			if headSHA == "" {
				headSHA = fc.PullRequestHeadSHA("acme", "widgets", 1)
			}
			srv.Hub().Broadcast(server.Event{
				Type: "pr_detail_refreshed",
				Data: map[string]any{
					"provider":      repo.Platform,
					"platform_host": repo.PlatformHost,
					"repo_path":     repo.RepoPath,
					"owner":         repo.Owner,
					"name":          repo.Name,
					"number":        1,
					"head_sha":      headSHA,
					"synced_at":     createdAt.Format(time.RFC3339),
					"warnings":      []string{},
				},
			})
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{
				"status": "reply-added",
			}); err != nil {
				slog.Warn("write e2e response", "err", err)
			}
			return
		}
		if r.Method == http.MethodPost &&
			strings.Contains(r.URL.Path, "/api/v1/repo/") &&
			strings.Contains(r.URL.Path, "/roborev-dev/") &&
			strings.HasSuffix(r.URL.Path, "/refresh") {
			r = r.WithContext(
				context.WithValue(r.Context(), globRefreshContextKey{}, true),
			)
		}
		srv.ServeHTTP(w, r)
	})

	// Do not start the syncer's background loop. The seeded DB is the
	// ground truth for E2E tests; RunOnce would overwrite it with
	// incomplete fixture client data. The syncer only needs to exist
	// for Status() and IsTrackedRepo() calls.

	built = true
	return &appState{
		tmpDir:      tmpDir,
		database:    database,
		srv:         srv,
		handler:     rootHandler,
		cfgPath:     cfgPath,
		worktreeDir: e2eWorktreeDir,
		tmuxCommand: tmuxCommand,
		tmuxGate:    tmuxGate,
		ptyOwner:    opts.preferPtyOwner,
		clones:      diffRepo.Manager,
	}, nil
}

type e2eFederationHandlerBox struct {
	handler http.Handler
}

type e2eFederationSwitch struct {
	current atomic.Pointer[e2eFederationHandlerBox]
	offline atomic.Bool
}

func newE2EFederationSwitch() *e2eFederationSwitch {
	switcher := &e2eFederationSwitch{}
	switcher.Set(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "federated e2e daemon is starting", http.StatusServiceUnavailable)
	}))
	return switcher
}

func (s *e2eFederationSwitch) Set(handler http.Handler) {
	s.current.Store(&e2eFederationHandlerBox{handler: handler})
}

func (s *e2eFederationSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.offline.Load() {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": http.StatusServiceUnavailable,
			"code":   "hubUnavailable",
			"detail": "the federated e2e hub is offline",
		})
		return
	}
	s.current.Load().handler.ServeHTTP(w, r)
}

func writeFederatedE2EControlResponse(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func runFederatedForgesE2E(
	ctx context.Context,
	roborevEndpoint, serverInfoFile string,
) error {
	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load frontend assets: %w", err)
	}

	hubSwitch := newE2EFederationSwitch()
	spokeASwitch := newE2EFederationSwitch()
	spokeBSwitch := newE2EFederationSwitch()
	hubHTTP := httptest.NewUnstartedServer(hubSwitch)
	spokeAHTTP := httptest.NewUnstartedServer(spokeASwitch)
	spokeBHTTP := httptest.NewUnstartedServer(spokeBSwitch)
	for _, origin := range []*httptest.Server{hubHTTP, spokeAHTTP, spokeBHTTP} {
		origin.StartTLS()
		defer origin.Close()
	}
	controlHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/hub/offline":
			hubSwitch.offline.Store(true)
			hubHTTP.CloseClientConnections()
			writeFederatedE2EControlResponse(w, "offline")
		case "/hub/online":
			hubSwitch.offline.Store(false)
			writeFederatedE2EControlResponse(w, "online")
		default:
			http.NotFound(w, r)
		}
	}))
	defer controlHTTP.Close()

	federationClient := e2eFederationHTTPClient(
		hubHTTP, spokeAHTTP, spokeBHTTP,
	)
	if transport, ok := federationClient.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	credentialDir, err := os.MkdirTemp("", "kenn-forge-e2e-federation-credentials-*")
	if err != nil {
		return fmt.Errorf("create federation credential directory: %w", err)
	}
	defer os.RemoveAll(credentialDir)
	hubCredentials, err := federationauth.Open(
		filepath.Join(credentialDir, "hub.json"),
	)
	if err != nil {
		return err
	}
	spokeACredentials, err := federationauth.Open(
		filepath.Join(credentialDir, "spoke-a.json"),
	)
	if err != nil {
		return err
	}
	spokeBCredentials, err := federationauth.Open(
		filepath.Join(credentialDir, "spoke-b.json"),
	)
	if err != nil {
		return err
	}
	hubEnrollments, err := federation.Open(
		filepath.Join(credentialDir, "hub-enrollments.json"),
		federation.StoreOptions{},
	)
	if err != nil {
		return err
	}
	spokeAEnrollments, err := federation.Open(
		filepath.Join(credentialDir, "spoke-a-enrollments.json"),
		federation.StoreOptions{},
	)
	if err != nil {
		return err
	}
	spokeBEnrollments, err := federation.Open(
		filepath.Join(credentialDir, "spoke-b-enrollments.json"),
		federation.StoreOptions{},
	)
	if err != nil {
		return err
	}
	if err := connectE2EFederationCredentials(
		hubCredentials, spokeACredentials,
		e2eHubNodeID, e2eSpokeANodeID,
	); err != nil {
		return err
	}
	if err := connectE2EFederationCredentials(
		hubCredentials, spokeBCredentials,
		e2eHubNodeID, e2eSpokeBNodeID,
	); err != nil {
		return err
	}
	if err := seedE2EFederationEnrollment(
		ctx, hubEnrollments, spokeAEnrollments,
		e2eHubNodeID, hubHTTP.URL, e2eSpokeANodeID, spokeAHTTP.URL,
	); err != nil {
		return err
	}
	if err := seedE2EFederationEnrollment(
		ctx, hubEnrollments, spokeBEnrollments,
		e2eHubNodeID, hubHTTP.URL, e2eSpokeBNodeID, spokeBHTTP.URL,
	); err != nil {
		return err
	}

	const (
		hubToken    = "federated-e2e-hub-local-token"
		spokeAToken = "federated-e2e-spoke-a-local-token"
		spokeBToken = "federated-e2e-spoke-b-local-token"
	)
	hubState, err := buildAppState(ctx, assets, appOptions{
		roborevEndpoint: roborevEndpoint, defaultPlatformHost: "github.com",
		nodeID: e2eHubNodeID,
		federation: &e2eFederationRuntime{
			fleet: config.Fleet{
				Enabled: true, Role: config.FleetRoleHub,
				BaseURL: hubHTTP.URL, PeerTimeout: "1s",
				Members: []config.FleetMember{
					{NodeID: e2eSpokeANodeID, Name: "Spoke A", BaseURL: spokeAHTTP.URL, State: federation.EnrollmentActive},
					{NodeID: e2eSpokeBNodeID, Name: "Spoke B", BaseURL: spokeBHTTP.URL, State: federation.EnrollmentActive},
				},
			},
			credentials: hubCredentials, enrollments: hubEnrollments,
			httpClient: federationClient,
			localToken: hubToken,
		},
	})
	if err != nil {
		return err
	}
	defer hubState.close()
	hubSwitch.Set(hubState.handler)

	spokeState := func(
		nodeID, baseURL, workspaceID, token string,
		credentials *federationauth.Store,
		enrollments *federation.Store,
		itemNumber int,
	) (*appState, error) {
		return buildAppState(ctx, assets, appOptions{
			roborevEndpoint: roborevEndpoint, defaultPlatformHost: "github.com",
			nodeID: nodeID,
			federation: &e2eFederationRuntime{
				fleet: config.Fleet{
					Enabled: true, Role: config.FleetRoleSpoke,
					BaseURL: baseURL, PeerTimeout: "1s",
					Hub: &config.FleetHub{
						NodeID: e2eHubNodeID, Name: "Hub",
						BaseURL: hubHTTP.URL,
					},
				},
				credentials: credentials, enrollments: enrollments,
				httpClient:  federationClient,
				activeSpoke: true, localToken: token,
				workspaceID: workspaceID, itemNumber: itemNumber,
			},
		})
	}
	spokeAState, err := spokeState(
		e2eSpokeANodeID, spokeAHTTP.URL, "federated-spoke-a-workspace",
		spokeAToken, spokeACredentials, spokeAEnrollments, 1,
	)
	if err != nil {
		return err
	}
	defer spokeAState.close()
	spokeASwitch.Set(spokeAState.handler)
	spokeBState, err := spokeState(
		e2eSpokeBNodeID, spokeBHTTP.URL, "federated-spoke-b-workspace",
		spokeBToken, spokeBCredentials, spokeBEnrollments, 2,
	)
	if err != nil {
		return err
	}
	defer spokeBState.close()
	spokeBSwitch.Set(spokeBState.handler)

	hubAddress, ok := hubHTTP.Listener.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected federation listener address %T", hubHTTP.Listener.Addr())
	}
	info := e2eServerInfo{
		Host: "127.0.0.1", Port: hubAddress.Port,
		BaseURL: hubHTTP.URL, PID: os.Getpid(),
		ConfigPath: hubState.cfgPath, NodeID: e2eHubNodeID,
		Federation: &e2eFederationInfo{
			HubURL:    hubHTTP.URL,
			SpokeAURL: spokeAHTTP.URL, SpokeBURL: spokeBHTTP.URL,
			ControlURL:  controlHTTP.URL,
			HubToken:    hubToken,
			SpokeAToken: spokeAToken, SpokeBToken: spokeBToken,
			HubNodeID:    e2eHubNodeID,
			SpokeANodeID: e2eSpokeANodeID, SpokeBNodeID: e2eSpokeBNodeID,
		},
	}
	if err := writeServerInfoFile(serverInfoFile, info); err != nil {
		return fmt.Errorf("write server info file: %w", err)
	}
	defer cleanupServerInfoFile(serverInfoFile)
	slog.Info("starting federated e2e servers",
		"hub", hubHTTP.URL,
		"spoke_a", spokeAHTTP.URL, "spoke_b", spokeBHTTP.URL,
	)
	<-ctx.Done()
	return nil
}

func e2eFederationHTTPClient(origins ...*httptest.Server) *http.Client {
	roots := x509.NewCertPool()
	for _, origin := range origins {
		roots.AddCert(origin.Certificate())
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, MinVersion: tls.VersionTLS12,
	}}, Timeout: 15 * time.Second}
}

func seedE2EFederationEnrollment(
	ctx context.Context,
	hub, spoke *federation.Store,
	hubNodeID, hubBaseURL, spokeNodeID, spokeBaseURL string,
) error {
	enrollment, err := federationtest.SeedActiveHubEnrollment(
		ctx, hub,
		federation.Identity{NodeID: hubNodeID, BaseURL: hubBaseURL},
		federation.Identity{NodeID: spokeNodeID, BaseURL: spokeBaseURL},
		spokeNodeID,
	)
	if err != nil {
		return err
	}
	return federationtest.SeedActiveSpokeEnrollment(ctx, spoke, enrollment)
}

func connectE2EFederationCredentials(
	hub, spoke *federationauth.Store,
	hubNodeID, nodeID string,
) error {
	spokeToHub, err := hub.MintInbound(
		nodeID, federationauth.SpokeToHubScopes(),
	)
	if err != nil {
		return err
	}
	if err := spoke.StoreOutbound(
		hubNodeID, spokeToHub,
		federationauth.SpokeToHubScopes(),
	); err != nil {
		return err
	}
	hubToSpoke, err := spoke.MintInbound(
		hubNodeID, federationauth.HubToSpokeScopes(),
	)
	if err != nil {
		return err
	}
	return hub.StoreOutbound(
		nodeID, hubToSpoke,
		federationauth.HubToSpokeScopes(),
	)
}

// run starts the e2e server and blocks until ctx is canceled or the
// HTTP server errors out. Tests call it directly with a cancellable
// context; main() wires it to SIGINT/SIGTERM.
func run(
	ctx context.Context,
	port int,
	roborevEndpoint, serverInfoFile, defaultPlatformHost string,
	visibleImportedModes bool,
	providerCollision bool,
) error {
	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load frontend assets: %w", err)
	}

	baseOpts := appOptions{
		roborevEndpoint:      roborevEndpoint,
		defaultPlatformHost:  defaultPlatformHost,
		visibleImportedModes: visibleImportedModes,
		providerCollision:    providerCollision,
		nodeID:               e2eStandaloneNodeID,
	}

	state, err := buildAppState(ctx, assets, baseOpts)
	if err != nil {
		return err
	}

	states := newAppStateRegistry(state)
	// Final cleanup of whichever state is live at exit. Runs last
	// (registered first): the httpServer/srv shutdown defers below
	// drain handlers before this closes the database and temp dir.
	defer func() {
		states.Load().close()
		states.waitForClosers()
	}()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("unexpected listener addr type %T", listener.Addr())
	}

	info := e2eServerInfo{
		Host:       "127.0.0.1",
		Port:       tcpAddr.Port,
		BaseURL:    fmt.Sprintf("http://127.0.0.1:%d", tcpAddr.Port),
		PID:        os.Getpid(),
		ConfigPath: state.cfgPath,
		NodeID:     e2eStandaloneNodeID,
	}

	// OTel export is opt-in via OTEL_TRACES_EXPORTER; a malformed value
	// must not take down the e2e suite, so warn and continue instead of
	// failing startup the way the primary server does.
	if otelShutdown, err := oteltelemetry.Init(ctx); err != nil {
		slog.Warn("e2e telemetry init failed", "err", err)
	} else {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelShutdown(shutdownCtx); err != nil {
				slog.Warn("e2e telemetry shutdown failed", "err", err)
			}
		}()
	}

	// The workspace-switch profiling harness sets KENN_FORGE_PPROF_ADDR
	// (typically 127.0.0.1:0) so it can capture Go-side pprof data for
	// the same window as the browser trace. Failure to bind must not
	// take down the e2e suite for an unrelated env var.
	if pprofAddr := strings.TrimSpace(os.Getenv("KENN_FORGE_PPROF_ADDR")); pprofAddr != "" {
		pprofSrv, pprofErr := profiler.Start(pprofAddr)
		if pprofErr != nil {
			slog.Warn("e2e pprof listener not started", "err", pprofErr)
		} else if pprofSrv != nil {
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := pprofSrv.Shutdown(shutdownCtx); err != nil {
					slog.Warn("e2e pprof listener shutdown failed", "err", err)
				}
			}()
			// Addr() is non-nil whenever Start succeeded, but its
			// signature allows nil; guard so nilaway can prove it.
			if addr := pprofSrv.Addr(); addr != nil {
				info.PprofAddr = addr.String()
				slog.Info(fmt.Sprintf("e2e pprof listener at http://%s/debug/pprof/", info.PprofAddr))
			}
		}
	}

	if err := writeServerInfoFile(serverInfoFile, info); err != nil {
		return fmt.Errorf("write server info file: %w", err)
	}
	defer cleanupServerInfoFile(serverInfoFile)

	slog.Info(fmt.Sprintf("starting e2e server at %s", info.BaseURL))

	// /__e2e/reset rebuilds the full fixture state in-process and
	// swaps it in, so Playwright can reuse one server process (and
	// port) across tests instead of spawning a fresh process per
	// test. The old state drains and cleans up in the background.
	var resetMu sync.Mutex
	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__e2e/reset" {
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", http.StatusMethodNotAllowed)
				return
			}
			resetMu.Lock()
			defer resetMu.Unlock()

			opts := baseOpts
			var req struct {
				DefaultPlatformHost  string `json:"default_platform_host"`
				VisibleImportedModes *bool  `json:"visible_imported_modes"`
				ProviderCollision    *bool  `json:"provider_collision"`
				PreferPtyOwner       *bool  `json:"prefer_pty_owner"`
			}
			// An empty body resets to the startup options; a
			// non-empty body must be valid JSON so option typos
			// fail loudly instead of silently resetting defaults.
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, "read reset body", http.StatusBadRequest)
				return
			}
			if len(bytes.TrimSpace(body)) > 0 {
				if err := json.Unmarshal(body, &req); err != nil {
					http.Error(
						w,
						fmt.Sprintf("invalid reset body: %v", err),
						http.StatusBadRequest,
					)
					return
				}
			}
			if strings.TrimSpace(req.DefaultPlatformHost) != "" {
				opts.defaultPlatformHost = req.DefaultPlatformHost
			}
			if req.VisibleImportedModes != nil {
				opts.visibleImportedModes = *req.VisibleImportedModes
			}
			if req.ProviderCollision != nil {
				opts.providerCollision = *req.ProviderCollision
			}
			if req.PreferPtyOwner != nil {
				opts.preferPtyOwner = *req.PreferPtyOwner
			}

			// Build against the process ctx, not r.Context(): a
			// client disconnect mid-build must not leave a
			// half-canceled state in the pool.
			newState, buildErr := buildAppState(ctx, assets, opts)
			if buildErr != nil {
				http.Error(
					w,
					fmt.Sprintf("reset: %v", buildErr),
					http.StatusInternalServerError,
				)
				return
			}
			old := states.Swap(newState)
			// Stop the old state's private tmux server synchronously. Its
			// remaining handler/database cleanup may continue off-request,
			// but a process exit cannot strand this test-owned daemon.
			old.stopTmux()
			// Remaining old-state teardown (handler drain and temp-dir
			// removal) happens off the request path, matching
			// the old SIGTERM-and-return stop() semantics.
			states.closeAsync(old.close)

			resetInfo := info
			resetInfo.ConfigPath = newState.cfgPath
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(resetInfo); err != nil {
				slog.Warn("write e2e reset response", "err", err)
			}
			return
		}
		states.ServeHTTP(w, r)
	})

	httpServer := &http.Server{
		Handler:     rootHandler,
		ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}

	// Drain HTTP handlers and bg goroutines before the deferred
	// state close above. srv.Shutdown closes the hub so SSE
	// handlers exit, then drains bg goroutines; httpServer.Shutdown
	// drains in-flight HTTP handlers.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := states.Load().srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("server shutdown", "err", err)
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http shutdown", "err", err)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.Serve(listener); !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		// Runner exit can close inherited stdio immediately after sending
		// SIGTERM. Release the private daemon before logging or draining so
		// that abrupt parent teardown cannot orphan it.
		states.Load().stopTmux()
		slog.Info("shutting down")
		// Trigger Shutdown so Serve unblocks (the defer is a
		// safety net for other exit paths and is idempotent).
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := states.Load().srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("server shutdown", "err", err)
		}
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			slog.Warn("http shutdown", "err", err)
		}
		// Drain errCh so a real Serve failure (not
		// ErrServerClosed) is surfaced instead of swallowed.
		if serveErr, ok := <-errCh; ok {
			return fmt.Errorf("server: %w", serveErr)
		}
		return nil
	case err := <-errCh:
		return fmt.Errorf("server: %w", err)
	}
}

func cleanupServerInfoFile(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("cleanup server info file failed", "path", path, "err", err)
	}
}

func writeServerInfoFile(path string, info e2eServerInfo) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir server info dir: %w", err)
	}

	content, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal server info: %w", err)
	}

	tmpPath := path + ".tmp"
	// Federation-mode server info contains local API bearers for its three
	// isolated daemons. Keep the file private even in a caller-supplied directory.
	if err := os.WriteFile(tmpPath, append(content, '\n'), 0o600); err != nil {
		return fmt.Errorf("write temp server info file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename server info file: %w", err)
	}
	return nil
}

func patchFixturePRSHAs(fc *testutil.FixtureClient, owner, repo string, number int, headSHA, baseSHA string) {
	if fc == nil {
		return
	}
	fc.UpdatePullRequestSHAs(owner, repo, number, headSHA, baseSHA)
}

func cleanupE2EWorkspaces(
	database *db.DB,
	clones *gitclone.Manager,
	worktreeDir string,
	tmuxCmd []string,
	preferPtyOwner bool,
) {
	if database == nil || worktreeDir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	manager := workspace.NewManager(database, worktreeDir)
	manager.SetTmuxCommand(tmuxCmd)
	if preferPtyOwner {
		manager.SetPtyOwnerClient(&ptyowner.Client{
			Root: filepath.Join(filepath.Dir(worktreeDir), "pty-owner"),
		})
	}
	if clones != nil {
		manager.SetClones(clones)
	}
	workspaces, err := manager.ListSummaries(ctx)
	if err != nil {
		slog.Warn("e2e workspace cleanup list failed", "err", err)
		return
	}
	for _, summary := range workspaces {
		if _, err := manager.Delete(ctx, summary.ID, true, nil); err != nil {
			slog.Warn(
				"e2e workspace cleanup delete failed",
				"workspace_id", summary.ID,
				"err", err,
			)
		}
	}
}
