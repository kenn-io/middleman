package stacks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	realdb "go.kenn.io/forge/internal/db"
	ghclient "go.kenn.io/forge/internal/github"
	"go.kenn.io/forge/platform"
)

func TestSyncCompletedHookUsesProviderQualifiedRepoIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()

	_, err := d.UpsertRepo(ctx, realdb.RepoIdentity{
		Platform:       "github",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "github-org-repo",
		Owner:          "org",
		Name:           "repo",
	})
	require.NoError(err)
	gitlabRepoID, err := d.UpsertRepo(ctx, realdb.RepoIdentity{
		Platform:       "gitlab",
		PlatformHost:   "code.example.com",
		PlatformRepoID: "gitlab-org-repo",
		Owner:          "org",
		Name:           "repo",
	})
	require.NoError(err)
	require.NoError(d.UpdateRepoProviderMetadata(ctx, gitlabRepoID, realdb.RepoProviderMetadata{
		CloneURL:      "https://code.example.com/org/repo.git",
		DefaultBranch: "main",
	}))

	now := time.Now().UTC()
	for i, pr := range []struct {
		number     int
		head, base string
	}{
		{100, "feature/base", "main"},
		{101, "feature/tip", "feature/base"},
	} {
		_, err := d.UpsertMergeRequest(ctx, &realdb.MergeRequest{
			RepoID: gitlabRepoID, PlatformID: int64(i + 1),
			Number: pr.number, Title: "MR", Author: "a", State: "open",
			HeadBranch: pr.head, BaseBranch: pr.base,
			HeadRepoCloneURL: "https://code.example.com/org/repo.git",
			CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
		})
		require.NoError(err)
	}

	SyncCompletedHook(ctx, d, nil)([]ghclient.RepoSyncResult{{
		Platform:     platform.KindGitLab,
		PlatformHost: "code.example.com",
		Owner:        "org",
		Name:         "repo",
	}})

	stacks, members, err := d.ListStacksWithMembers(ctx, "")
	require.NoError(err)
	require.Len(stacks, 1)
	assert.Equal(gitlabRepoID, stacks[0].RepoID)
	assert.Len(members[stacks[0].ID], 2)
}

// TestSyncCompletedHookDistinguishesPartialScopeFailures pins that stack
// detection skips a repo only when the failure affects merge-request data:
// an issue-scope partial failure must not leave stacks stale, while
// MR-scope partial failures and hard repository failures keep the skip.
func TestSyncCompletedHookDistinguishesPartialScopeFailures(t *testing.T) {
	cases := []struct {
		name        string
		result      ghclient.RepoSyncResult
		wantStacks  int
		description string
	}{
		{
			name: "issue-scope partial failure still detects stacks",
			result: ghclient.RepoSyncResult{
				Error:          "one or more issue sync items failed",
				PartialFailure: &ghclient.PartialSyncError{Issues: true},
			},
			wantStacks: 1,
		},
		{
			name: "merge-request-scope partial failure skips",
			result: ghclient.RepoSyncResult{
				Error:          "one or more merge request sync items failed",
				PartialFailure: &ghclient.PartialSyncError{MergeRequests: true},
			},
			wantStacks: 0,
		},
		{
			name:       "hard repository failure skips",
			result:     ghclient.RepoSyncResult{Error: "list open PRs: boom"},
			wantStacks: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			d := openTestDB(t)
			ctx := t.Context()

			repoID, err := d.UpsertRepo(ctx, realdb.RepoIdentity{
				Platform:       "github",
				PlatformHost:   "github.com",
				PlatformRepoID: "github-org-repo",
				Owner:          "org",
				Name:           "repo",
			})
			require.NoError(err)
			require.NoError(d.UpdateRepoProviderMetadata(ctx, repoID, realdb.RepoProviderMetadata{
				CloneURL:      "https://github.com/org/repo.git",
				DefaultBranch: "main",
			}))
			now := time.Now().UTC()
			for i, pr := range []struct {
				number     int
				head, base string
			}{
				{100, "feature/base", "main"},
				{101, "feature/tip", "feature/base"},
			} {
				_, err := d.UpsertMergeRequest(ctx, &realdb.MergeRequest{
					RepoID: repoID, PlatformID: int64(i + 1),
					Number: pr.number, Title: "MR", Author: "a", State: "open",
					HeadBranch: pr.head, BaseBranch: pr.base,
					HeadRepoCloneURL: "https://github.com/org/repo.git",
					CreatedAt:        now, UpdatedAt: now, LastActivityAt: now,
				})
				require.NoError(err)
			}

			result := tc.result
			result.Platform = platform.KindGitHub
			result.PlatformHost = "github.com"
			result.Owner = "org"
			result.Name = "repo"
			SyncCompletedHook(ctx, d, nil)([]ghclient.RepoSyncResult{result})

			stacks, _, err := d.ListStacksWithMembers(ctx, "")
			require.NoError(err)
			assert.Len(t, stacks, tc.wantStacks)
		})
	}
}
