package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertTestMRWithBranches(
	t *testing.T,
	d *DB,
	repoID int64,
	number int,
	head, base string,
	state MergeRequestState,
	opts ...testMROpt,
) int64 {
	t.Helper()
	allOpts := []testMROpt{withMRTitle("PR " + head), withMRBranches(head, base), withMRState(state)}
	allOpts = append(allOpts, opts...)
	return insertTestMRWithOptions(t, d, testMR(repoID, number, allOpts...))
}

func TestListPRsForStackDetection(t *testing.T) {
	assert := assert.New(t)
	d := openTestDB(t)
	repoID := insertTestRepo(t, d, "org", "repo")

	// open PR — included
	insertTestMRWithBranches(t, d, repoID, 1, "feature/a", "main", MergeRequestStateOpen)
	// merged PR — included
	insertTestMRWithBranches(t, d, repoID, 2, "feature/b", "feature/a", MergeRequestStateMerged)
	// closed PR — excluded
	insertTestMRWithBranches(t, d, repoID, 3, "feature/c", "main", MergeRequestStateClosed)

	prs, err := d.ListPRsForStackDetection(t.Context(), repoID)
	require.NoError(t, err)
	assert.Len(prs, 2)
	numbers := []int{prs[0].Number, prs[1].Number}
	assert.ElementsMatch([]int{1, 2}, numbers)
}

func TestUpsertStackAndReplaceMembers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID1 := insertTestMRWithBranches(t, d, repoID, 1, "feature/a", "main", "open")
	mrID2 := insertTestMRWithBranches(t, d, repoID, 2, "feature/b", "feature/a", "open")

	// Create stack (keyed by repo_id + base_number)
	stackID, err := d.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	assert.Positive(stackID)

	// Idempotent upsert returns same ID
	stackID2, err := d.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	assert.Equal(stackID, stackID2)

	// Replace members
	members := []StackMember{
		{StackID: stackID, MergeRequestID: mrID1, Position: 1},
		{StackID: stackID, MergeRequestID: mrID2, Position: 2},
	}
	err = d.ReplaceStackMembers(ctx, stackID, members)
	require.NoError(err)

	// Verify via ListStacksWithMembers
	stacks, memberMap, err := d.ListStacksWithMembers(ctx, "")
	require.NoError(err)
	assert.Len(stacks, 1)
	assert.Equal("feature", stacks[0].Name)
	assert.Equal("org", stacks[0].RepoOwner)
	assert.Equal("repo", stacks[0].RepoName)
	assert.Len(memberMap[stackID], 2)
	assert.Equal(1, memberMap[stackID][0].Position)
	assert.Equal(2, memberMap[stackID][1].Position)
}

func TestStackMembersRenumberAfterRemovedMembersAreFiltered(t *testing.T) {
	tests := []struct {
		name          string
		removedNumber int
		wantNumbers   []int
	}{
		{name: "first member", removedNumber: 1, wantNumbers: []int{2, 3}},
		{name: "middle member", removedNumber: 2, wantNumbers: []int{1, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			d := openTestDB(t)
			ctx := t.Context()
			repoID := insertTestRepo(t, d, "org", "repo")

			memberIDs := make([]int64, 0, 3)
			for number := 1; number <= 3; number++ {
				memberIDs = append(memberIDs, insertTestMRWithBranches(
					t, d, repoID, number,
					fmt.Sprintf("feature/%d", number), "main", MergeRequestStateOpen,
				))
			}
			stackID, err := d.UpsertStack(ctx, repoID, 1, "feature")
			require.NoError(err)
			require.NoError(d.ReplaceStackMembers(ctx, stackID, []StackMember{
				{MergeRequestID: memberIDs[0], Position: 1},
				{MergeRequestID: memberIDs[1], Position: 2},
				{MergeRequestID: memberIDs[2], Position: 3},
			}))

			now := time.Now().UTC().Truncate(time.Second)
			_, err = d.WriteDB().ExecContext(ctx, `
				INSERT INTO forge_archive_items (
					repo_id, item_type, item_number, provider_item_id,
					provider_created_at, provider_updated_at, lifecycle_state
				) VALUES (?, 'merge_request', ?, ?, ?, ?, 'removed_upstream')`,
				repoID, tt.removedNumber,
				fmt.Sprintf("pull-%d", tt.removedNumber), now, now,
			)
			require.NoError(err)

			_, memberMap, err := d.ListStacksWithMembers(ctx, "")
			require.NoError(err)
			require.Len(memberMap[stackID], 2)
			for i, member := range memberMap[stackID] {
				require.Equal(tt.wantNumbers[i], member.Number)
				require.Equal(i+1, member.Position)
			}

			stack, members, err := d.GetStackForPRByRepoID(
				ctx, repoID, tt.wantNumbers[1],
			)
			require.NoError(err)
			require.NotNil(stack)
			require.Len(members, 2)
			for i, member := range members {
				require.Equal(tt.wantNumbers[i], member.Number)
				require.Equal(i+1, member.Position)
			}
		})
	}
}

func TestStackMembersIncludeMergeableState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID1 := insertTestMRWithBranches(
		t, d, repoID, 1, "feature/a", "main", "open", withMRMergeableState("dirty"),
	)
	mrID2 := insertTestMRWithBranches(t, d, repoID, 2, "feature/b", "feature/a", "open")
	stackID, err := d.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackID, []StackMember{
		{StackID: stackID, MergeRequestID: mrID1, Position: 1},
		{StackID: stackID, MergeRequestID: mrID2, Position: 2},
	})
	require.NoError(err)

	_, members, err := d.GetStackForPRByRepoID(ctx, repoID, 2)
	require.NoError(err)
	require.Len(members, 2)
	assert.Equal("dirty", members[0].MergeableState)
	assert.Empty(members[1].MergeableState)
}

func TestListMRsBlockedByStackConflicts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID1 := insertTestMRWithBranches(
		t, d, repoID, 1, "feature/a", "main", "open", withMRMergeableState("dirty"),
	)
	mrID2 := insertTestMRWithBranches(t, d, repoID, 2, "feature/b", "feature/a", "open")
	mrID3 := insertTestMRWithBranches(t, d, repoID, 3, "feature/c", "feature/b", "open")
	stackID, err := d.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackID, []StackMember{
		{StackID: stackID, MergeRequestID: mrID1, Position: 1},
		{StackID: stackID, MergeRequestID: mrID2, Position: 2},
		{StackID: stackID, MergeRequestID: mrID3, Position: 3},
	})
	require.NoError(err)

	blocked, err := d.ListMRsBlockedByStackConflicts(ctx, []int64{mrID1, mrID2, mrID3})
	require.NoError(err)
	assert.False(blocked[mrID1])
	assert.True(blocked[mrID2])
	assert.True(blocked[mrID3])
}

func TestListStackPlacementsForMRs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID1 := insertTestMRWithBranches(t, d, repoID, 1, "feature/a", "main", "merged")
	mrID2 := insertTestMRWithBranches(t, d, repoID, 2, "feature/b", "feature/a", "open")
	mrID3 := insertTestMRWithBranches(t, d, repoID, 3, "feature/c", "feature/b", "open")
	mrID4 := insertTestMRWithBranches(t, d, repoID, 4, "feature/d", "feature/c", "open")
	soloID := insertTestMRWithBranches(t, d, repoID, 9, "solo", "main", "open")
	stackID, err := d.UpsertStack(ctx, repoID, 1, "feature")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackID, []StackMember{
		{StackID: stackID, MergeRequestID: mrID1, Position: 1},
		{StackID: stackID, MergeRequestID: mrID2, Position: 2},
		{StackID: stackID, MergeRequestID: mrID3, Position: 3},
		{StackID: stackID, MergeRequestID: mrID4, Position: 4},
	})
	require.NoError(err)

	placements, err := d.ListStackPlacementsForMRs(ctx, []int64{mrID2, mrID4, soloID})
	require.NoError(err)
	assert.Equal(StackPlacement{Position: 2, Size: 4}, placements[mrID2])
	assert.Equal(StackPlacement{Position: 4, Size: 4}, placements[mrID4])
	_, requested := placements[mrID1]
	assert.False(requested, "unrequested members must not be returned")
	_, inStack := placements[soloID]
	assert.False(inStack, "merge requests outside a stack must not be returned")

	// Hiding a member removed upstream renumbers the visible members.
	_, err = d.WriteDB().ExecContext(ctx, `
		INSERT INTO forge_archive_items (
			repo_id, item_type, item_number, provider_item_id,
			provider_created_at, provider_updated_at, lifecycle_state
		) VALUES (?, 'merge_request', 2, 'pull-2', ?, ?, 'removed_upstream')`,
		repoID, baseTime(), baseTime(),
	)
	require.NoError(err)

	placements, err = d.ListStackPlacementsForMRs(ctx, []int64{mrID2, mrID3, mrID4})
	require.NoError(err)
	_, hidden := placements[mrID2]
	assert.False(hidden, "hidden members must not be returned")
	assert.Equal(StackPlacement{Position: 2, Size: 3}, placements[mrID3])
	assert.Equal(StackPlacement{Position: 3, Size: 3}, placements[mrID4])

	empty, err := d.ListStackPlacementsForMRs(ctx, nil)
	require.NoError(err)
	assert.Empty(empty)

	// The pull list has no size cap, so the lookup must survive an ID set far
	// beyond SQLite's bind-parameter ceiling (32,766 variables).
	large := make([]int64, 0, 70_000)
	for id := int64(1_000_000); id < 1_070_000; id++ {
		large = append(large, id)
	}
	large = append(large, mrID3, mrID4)
	placements, err = d.ListStackPlacementsForMRs(ctx, large)
	require.NoError(err)
	assert.Len(placements, 2)
	assert.Equal(StackPlacement{Position: 2, Size: 3}, placements[mrID3])
}

func TestDeleteStaleStacks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	id1, err := d.UpsertStack(ctx, repoID, 1, "keep")
	require.NoError(err)
	_, err = d.UpsertStack(ctx, repoID, 2, "delete-me")
	require.NoError(err)

	err = d.DeleteStaleStacks(ctx, repoID, []int64{id1})
	require.NoError(err)

	// Verify directly that "delete-me" is gone.
	var count int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_stacks WHERE repo_id = ?`, repoID,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(1, count) // only "keep" remains
}

func TestListStacksWithMembers_MalformedFilter(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()

	for _, bad := range []string{"noslash", "/bar", "foo/", "/", "foo/bar/baz"} {
		_, _, err := d.ListStacksWithMembers(ctx, bad)
		require.Error(t, err, "filter=%q should fail", bad)
	}
	// Empty string is valid (no filter).
	_, _, err := d.ListStacksWithMembers(ctx, "")
	require.NoError(t, err)
}

func TestReplaceStackMembersReassignsAcrossStacks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID := insertTestMRWithBranches(t, d, repoID, 1, "feature/a", "main", "open")

	// Put PR in stackA.
	stackA, err := d.UpsertStack(ctx, repoID, 1, "stackA")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackA, []StackMember{
		{StackID: stackA, MergeRequestID: mrID, Position: 1},
	})
	require.NoError(err)

	// Reassigning same PR to stackB should succeed by evicting from stackA.
	stackB, err := d.UpsertStack(ctx, repoID, 2, "stackB")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackB, []StackMember{
		{StackID: stackB, MergeRequestID: mrID, Position: 1},
	})
	require.NoError(err)

	// Only one membership row remains, now in stackB.
	var count int
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_stack_members WHERE merge_request_id = ?`,
		mrID,
	).Scan(&count)
	require.NoError(err)
	assert.Equal(1, count)

	var gotStack int64
	err = d.ReadDB().QueryRowContext(ctx,
		`SELECT stack_id FROM forge_stack_members WHERE merge_request_id = ?`,
		mrID,
	).Scan(&gotStack)
	require.NoError(err)
	assert.Equal(stackB, gotStack)
}

func TestGetStackForPR(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	d := openTestDB(t)
	ctx := t.Context()
	repoID := insertTestRepo(t, d, "org", "repo")

	mrID1 := insertTestMRWithBranches(t, d, repoID, 10, "feature/a", "main", "open")
	mrID2 := insertTestMRWithBranches(t, d, repoID, 11, "feature/b", "feature/a", "open")

	stackID, err := d.UpsertStack(ctx, repoID, 10, "feature")
	require.NoError(err)
	err = d.ReplaceStackMembers(ctx, stackID, []StackMember{
		{StackID: stackID, MergeRequestID: mrID1, Position: 1},
		{StackID: stackID, MergeRequestID: mrID2, Position: 2},
	})
	require.NoError(err)

	// Found
	stack, members, err := d.GetStackForPR(ctx, "github", "github.com", "org", "repo", 10)
	require.NoError(err)
	require.NotNil(stack)
	assert.Equal("feature", stack.Name)
	assert.Len(members, 2)

	// Not found
	stack2, _, err := d.GetStackForPR(ctx, "github", "github.com", "org", "repo", 999)
	require.NoError(err)
	assert.Nil(stack2)
}
