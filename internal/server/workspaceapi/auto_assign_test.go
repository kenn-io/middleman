package workspaceapi

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/internal/providerplane"
	"go.kenn.io/forge/internal/server/httpapi"
	"go.kenn.io/forge/internal/testutil/dbtest"
	"go.kenn.io/forge/platform"
)

type spokePreparationRejectingAdmitter struct{}

func (spokePreparationRejectingAdmitter) Admit(context.Context) (func(), error) {
	return nil, providerplane.ErrSpokePreparationInProgress
}

type autoAssignProvider struct {
	pull           platform.MergeRequest
	issue          platform.Issue
	pullAssigned   []string
	issueAssigned  []string
	listPullCalls  int
	getPullCalls   int
	listIssueCalls int
	getIssueCalls  int
}

func (p *autoAssignProvider) Platform() platform.Kind { return platform.KindGitLab }
func (p *autoAssignProvider) Host() string            { return "git.example.test" }
func (p *autoAssignProvider) Capabilities() platform.Capabilities {
	return platform.Capabilities{
		ReadMergeRequests:     true,
		ReadIssues:            true,
		ReadAuthenticatedUser: true,
		AssigneeMutation:      true,
	}
}
func (p *autoAssignProvider) AuthenticatedUser(context.Context, platform.RepoRef) (string, error) {
	return "maintainer", nil
}
func (p *autoAssignProvider) ListOpenMergeRequests(context.Context, platform.RepoRef) ([]platform.MergeRequest, error) {
	p.listPullCalls++
	return nil, nil
}
func (p *autoAssignProvider) GetMergeRequest(context.Context, platform.RepoRef, int) (platform.MergeRequest, error) {
	p.getPullCalls++
	return p.pull, nil
}
func (p *autoAssignProvider) ListMergeRequestEvents(context.Context, platform.RepoRef, int) ([]platform.MergeRequestEvent, error) {
	return nil, nil
}
func (p *autoAssignProvider) ListOpenIssues(context.Context, platform.RepoRef) ([]platform.Issue, error) {
	p.listIssueCalls++
	return nil, nil
}
func (p *autoAssignProvider) GetIssue(context.Context, platform.RepoRef, int) (platform.Issue, error) {
	p.getIssueCalls++
	return p.issue, nil
}
func (p *autoAssignProvider) ListIssueEvents(context.Context, platform.RepoRef, int) ([]platform.IssueEvent, error) {
	return nil, nil
}
func (p *autoAssignProvider) SetMergeRequestAssignees(
	_ context.Context, _ platform.RepoRef, _ int, usernames []string,
) ([]string, error) {
	p.pullAssigned = slices.Clone(usernames)
	return slices.Clone(usernames), nil
}
func (p *autoAssignProvider) SetIssueAssignees(
	_ context.Context, _ platform.RepoRef, _ int, usernames []string,
) ([]string, error) {
	p.issueAssigned = slices.Clone(usernames)
	return slices.Clone(usernames), nil
}

func TestAutoAssignWorkspaceItemPreservesExistingAssignees(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	database := dbtest.Open(t)
	repoIdentity := db.RepoIdentity{
		Platform:       string(platform.KindGitLab),
		PlatformHost:   "git.example.test",
		PlatformRepoID: "repo-acme-widget",
		Owner:          "acme",
		Name:           "widget",
	}
	repoID, err := database.UpsertRepo(t.Context(), repoIdentity)
	require.NoError(err)
	now := time.Now().UTC().Truncate(time.Second)
	pullID, err := database.UpsertMergeRequest(t.Context(), &db.MergeRequest{
		RepoID: repoID, PlatformID: 101, Number: 7, URL: "https://git.example.test/acme/widget/merge_requests/7",
		Title: "Improve widget", Author: "author", State: db.MergeRequestStateOpen,
		HeadBranch: "feature", BaseBranch: "main", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		Assignees: []string{"reviewer"},
	})
	require.NoError(err)
	issueID, err := database.UpsertIssue(t.Context(), &db.Issue{
		RepoID: repoID, PlatformID: 102, Number: 8, URL: "https://git.example.test/acme/widget/issues/8",
		Title: "Fix widget", Author: "author", State: "open", CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
		Assignees: []string{"reviewer"},
	})
	require.NoError(err)

	provider := &autoAssignProvider{
		pull:  platform.MergeRequest{Assignees: []string{"reviewer"}},
		issue: platform.Issue{Assignees: []string{"reviewer"}},
	}
	registry, err := platform.NewRegistry(provider)
	require.NoError(err)
	syncer := ghclient.NewSyncerWithRegistry(registry, database, nil, nil, time.Hour, nil, nil)
	t.Cleanup(syncer.Stop)
	handler := New(Deps{
		DB:       database,
		Resolver: httpapi.NewRepositoryResolver(httpapi.RepositoryResolverDeps{DB: database}),
		Syncer:   syncer,
		Config:   ConfigSnapshot{AutoAssignOnCreate: true},
	})
	repo, err := database.GetRepoByIdentity(t.Context(), repoIdentity)
	require.NoError(err)
	require.NotNil(repo)

	tests := []struct {
		name     string
		number   int
		issue    bool
		assigned func() []string
		stored   func() []string
	}{
		{
			name: "pull request", number: 7,
			assigned: func() []string { return provider.pullAssigned },
			stored: func() []string {
				item, getErr := database.GetMergeRequestByRepoIDAndNumber(t.Context(), repoID, 7)
				require.NoError(getErr)
				require.NotNil(item)
				assert.Equal(pullID, item.ID)
				return item.Assignees
			},
		},
		{
			name: "issue", number: 8, issue: true,
			assigned: func() []string { return provider.issueAssigned },
			stored: func() []string {
				item, getErr := database.GetIssueByRepoIDAndNumber(t.Context(), repoID, 8)
				require.NoError(getErr)
				require.NotNil(item)
				assert.Equal(issueID, item.ID)
				return item.Assignees
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(handler.autoAssignWorkspaceItem(t.Context(), *repo, tt.number, tt.issue, false))
			assert.Equal([]string{"reviewer", "maintainer"}, tt.assigned())
			assert.Equal([]string{"reviewer", "maintainer"}, tt.stored())

			provider.pullAssigned = nil
			provider.issueAssigned = nil
			require.NoError(handler.autoAssignWorkspaceItem(t.Context(), *repo, tt.number, tt.issue, true))
			assert.Nil(tt.assigned())
		})
	}

	// The spoke owns the auto-assignment preference and calls this service only
	// after applying it. The hub must execute that request even when
	// its own local workspace preference differs.
	handler.config.AutoAssignOnCreate = false
	provider.pullAssigned = nil
	require.NoError(handler.AutoAssignProviderWorkspaceItem(
		t.Context(), ProviderWorkspaceItemRequest{
			Repository: providerplane.RepositoryRoute{
				Provider: string(platform.KindGitLab), PlatformHost: "git.example.test",
				Owner: "acme", Name: "widget",
			},
			ItemType: db.WorkspaceItemTypePullRequest, ItemNumber: 7,
		},
	))
	assert.Equal([]string{"reviewer", "maintainer"}, provider.pullAssigned)
	handler.config.AutoAssignOnCreate = true

	_, err = database.WriteDB().ExecContext(t.Context(), `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES
			(?, 'merge_request', 7, 'pull-7', ?, ?, 'removed_upstream'),
			(?, 'issue', 8, 'issue-8', ?, ?, 'removed_upstream')`,
		repoID, now, now, repoID, now, now,
	)
	require.NoError(err)
	provider.pullAssigned = nil
	provider.issueAssigned = nil

	err = handler.autoAssignWorkspaceItem(t.Context(), *repo, 7, false, false)
	require.ErrorContains(err, "not visible")
	err = handler.autoAssignWorkspaceItem(t.Context(), *repo, 8, true, false)
	require.ErrorContains(err, "not visible")
	assert.Empty(provider.pullAssigned)
	assert.Empty(provider.issueAssigned)
}

func TestSpokePreparationBlocksWorkspaceAutoAssignBeforeProviderAccess(t *testing.T) {
	handler := &Handler{
		syncer:            &ghclient.Syncer{},
		config:            ConfigSnapshot{AutoAssignOnCreate: true},
		providerWriteGate: spokePreparationRejectingAdmitter{},
	}
	err := handler.autoAssignWorkspaceItem(
		t.Context(), db.Repo{}, 7, false, false,
	)
	require.ErrorIs(t, err, providerplane.ErrSpokePreparationInProgress)
}
