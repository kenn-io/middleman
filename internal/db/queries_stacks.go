package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ListPRsForStackDetection returns non-closed PRs for a repo (open + merged).
func (d *DB) ListPRsForStackDetection(ctx context.Context, repoID int64) ([]MergeRequest, error) {
	return d.listPRsForStacks(ctx, repoID, `AND state IN ('open', 'merged')`)
}

// ListPRsForNativeStackMembers returns every PR for a repo regardless of state.
// A provider-authoritative stack can keep a closed, unmerged member, and
// dropping that row would leave the whole stack unresolvable.
func (d *DB) ListPRsForNativeStackMembers(ctx context.Context, repoID int64) ([]MergeRequest, error) {
	return d.listPRsForStacks(ctx, repoID, "")
}

func (d *DB) listPRsForStacks(ctx context.Context, repoID int64, stateFilter string) ([]MergeRequest, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, number, title, head_branch, base_branch, state, ci_status, review_decision,
		       head_repo_clone_url
		FROM forge_merge_requests
		WHERE repo_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = forge_merge_requests.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = forge_merge_requests.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  ) `+stateFilter+`
		ORDER BY number`,
		repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("list prs for stack detection: %w", err)
	}
	defer rows.Close()

	var prs []MergeRequest
	for rows.Next() {
		var mr MergeRequest
		mr.RepoID = repoID
		if err := rows.Scan(
			&mr.ID, &mr.Number, &mr.Title, &mr.HeadBranch, &mr.BaseBranch,
			&mr.State, &mr.CIStatus, &mr.ReviewDecision,
			&mr.HeadRepoCloneURL,
		); err != nil {
			return nil, fmt.Errorf("scan pr for stack detection: %w", err)
		}
		prs = append(prs, mr)
	}
	return prs, rows.Err()
}

// UpsertStack inserts or updates a stack keyed by (repo_id, base_number).
func (d *DB) UpsertStack(ctx context.Context, repoID int64, baseNumber int, name string) (int64, error) {
	_, err := d.execContext(ctx, `
		INSERT INTO forge_stacks (repo_id, base_number, name)
		VALUES (?, ?, ?)
		ON CONFLICT(repo_id, base_number) DO UPDATE SET
			name = excluded.name, updated_at = datetime('now')`,
		repoID, baseNumber, name,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert stack: %w", err)
	}
	var id int64
	err = d.roQueryRowContext(ctx,
		`SELECT id FROM forge_stacks WHERE repo_id = ? AND base_number = ?`,
		repoID, baseNumber,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("get stack id: %w", err)
	}
	return id, nil
}

// ReplaceStackMembers atomically replaces all members of a stack.
// Also removes the new members from any other stack they might belong to,
// so PRs can be reassigned between stacks without violating the unique
// merge_request_id constraint.
func (d *DB) ReplaceStackMembers(ctx context.Context, stackID int64, members []StackMember) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM forge_stack_members WHERE stack_id = ?`, stackID,
		); err != nil {
			return fmt.Errorf("delete old stack members: %w", err)
		}
		if len(members) == 0 {
			return nil
		}
		// Evict these PRs from any other stack to avoid unique-index conflict.
		for _, m := range members {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM forge_stack_members WHERE merge_request_id = ?`,
				m.MergeRequestID,
			); err != nil {
				return fmt.Errorf("evict existing stack member: %w", err)
			}
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO forge_stack_members (stack_id, merge_request_id, position)
			VALUES (?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare insert stack member: %w", err)
		}
		defer stmt.Close()
		for _, m := range members {
			if _, err := stmt.ExecContext(ctx, stackID, m.MergeRequestID, m.Position); err != nil {
				return fmt.Errorf("insert stack member: %w", err)
			}
		}
		return nil
	})
}

// ListStacksWithMembers returns stacks with repo info and their members.
// Only stacks that have at least one open member are returned.
func (d *DB) ListStacksWithMembers(ctx context.Context, repoFilter string) ([]StackWithRepo, map[int64][]StackMemberWithPR, error) {
	var conds []string
	var args []any
	if repoFilter != "" {
		pathKey := canonicalRepoPathKey(repoFilter)
		if pathKey == "" || !strings.Contains(pathKey, "/") {
			return nil, nil, fmt.Errorf("invalid repo filter %q: expected owner/name", repoFilter)
		}
		if strings.Count(pathKey, "/") > 1 {
			var exists int
			err := d.roQueryRowContext(ctx,
				`SELECT 1 FROM forge_repos
				 WHERE repo_path_key = ? AND lifecycle_state = 'active'
				 LIMIT 1`,
				pathKey,
			).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("invalid repo filter %q: expected owner/name", repoFilter)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("validate repo filter: %w", err)
			}
		}
		conds = append(conds, "r.repo_path_key = ?")
		args = append(args, pathKey)
	}
	conds = append(conds, `EXISTS (
		SELECT 1 FROM forge_stack_members sm2
		JOIN forge_merge_requests p2 ON p2.id = sm2.merge_request_id
		WHERE sm2.stack_id = s.id AND p2.state = 'open'
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = p2.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = p2.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  ))`)
	conds = append(conds, "r.lifecycle_state = 'active'")

	where := "WHERE " + strings.Join(conds, " AND ")

	stackQuery := fmt.Sprintf(`
		SELECT s.id, s.repo_id, s.base_number, s.name, s.created_at, s.updated_at,
		       r.owner, r.name
		FROM forge_stacks s
		JOIN forge_repos r ON r.id = s.repo_id
		%s
		ORDER BY s.updated_at DESC`, where)

	rows, err := d.roQueryContext(ctx, stackQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list stacks: %w", err)
	}
	defer rows.Close()

	var stacks []StackWithRepo
	var stackIDs []int64
	for rows.Next() {
		var s StackWithRepo
		if err := rows.Scan(
			&s.ID, &s.RepoID, &s.BaseNumber, &s.Name, &s.CreatedAt, &s.UpdatedAt,
			&s.RepoOwner, &s.RepoName,
		); err != nil {
			return nil, nil, fmt.Errorf("scan stack: %w", err)
		}
		stacks = append(stacks, s)
		stackIDs = append(stackIDs, s.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(stackIDs) == 0 {
		return stacks, make(map[int64][]StackMemberWithPR), nil
	}

	memberArgs := make([]any, len(stackIDs))
	for i, id := range stackIDs {
		memberArgs[i] = id
	}
	memberQuery := `
		SELECT sm.stack_id, sm.merge_request_id, sm.position,
		       p.number, p.title, p.state, p.ci_status, p.review_decision,
		       p.is_draft, p.base_branch, p.mergeable_state
		FROM forge_stack_members sm
		JOIN forge_merge_requests p ON p.id = sm.merge_request_id
		WHERE sm.stack_id IN (` + sqlPlaceholders(len(stackIDs)) + `)
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = p.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = p.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  )
		ORDER BY sm.stack_id, sm.position`

	mRows, err := d.roQueryContext(ctx, memberQuery, memberArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("list stack members: %w", err)
	}
	defer mRows.Close()

	memberMap := make(map[int64][]StackMemberWithPR)
	for mRows.Next() {
		var m StackMemberWithPR
		if err := mRows.Scan(
			&m.StackID, &m.MergeRequestID, &m.Position,
			&m.Number, &m.Title, &m.State, &m.CIStatus, &m.ReviewDecision,
			&m.IsDraft, &m.BaseBranch, &m.MergeableState,
		); err != nil {
			return nil, nil, fmt.Errorf("scan stack member: %w", err)
		}
		m.Position = len(memberMap[m.StackID]) + 1
		memberMap[m.StackID] = append(memberMap[m.StackID], m)
	}
	return stacks, memberMap, mRows.Err()
}

// DeleteStaleStacks removes stacks for a repo that are not in the active set.
func (d *DB) DeleteStaleStacks(ctx context.Context, repoID int64, activeStackIDs []int64) error {
	if len(activeStackIDs) == 0 {
		_, err := d.execContext(ctx,
			`DELETE FROM forge_stacks WHERE repo_id = ?`, repoID)
		if err != nil {
			return fmt.Errorf("delete all stacks for repo: %w", err)
		}
		return nil
	}
	args := make([]any, 0, len(activeStackIDs)+1)
	args = append(args, repoID)
	for _, id := range activeStackIDs {
		args = append(args, id)
	}
	_, err := d.execContext(ctx,
		`DELETE FROM forge_stacks WHERE repo_id = ? AND id NOT IN (`+
			sqlPlaceholders(len(activeStackIDs))+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("delete stale stacks: %w", err)
	}
	return nil
}

// GetStackForPR returns the stack and members for a specific PR, or nil if not in a stack.
func (d *DB) GetStackForPR(
	ctx context.Context,
	platform, platformHost, owner, name string,
	number int,
) (*Stack, []StackMemberWithPR, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	return d.getStackForPRWhere(
		ctx,
		`WHERE r.platform = ? AND r.platform_host = ?
		    AND r.owner_key = ? AND r.name_key = ?
		    AND p.number = ?
		    AND NOT EXISTS (
		        SELECT 1 FROM forge_archive_items ai
		        WHERE ai.repo_id = p.repo_id
		          AND ai.item_type = 'merge_request'
		          AND ai.item_number = p.number
		          AND ai.lifecycle_state = 'removed_upstream'
		    )`,
		platform, platformHost, owner, name, number,
	)
}

// GetStackForPRByRepoID returns the stack and members for a specific PR within
// one repository, or nil if the PR is not in a stack.
func (d *DB) GetStackForPRByRepoID(ctx context.Context, repoID int64, number int) (*Stack, []StackMemberWithPR, error) {
	return d.getStackForPRWhere(
		ctx,
		`WHERE p.repo_id = ? AND p.number = ?
		    AND NOT EXISTS (
		        SELECT 1 FROM forge_archive_items ai
		        WHERE ai.repo_id = p.repo_id
		          AND ai.item_type = 'merge_request'
		          AND ai.item_number = p.number
		          AND ai.lifecycle_state = 'removed_upstream'
		    )`,
		repoID, number,
	)
}

func (d *DB) getStackForPRWhere(ctx context.Context, where string, args ...any) (*Stack, []StackMemberWithPR, error) {
	var stack Stack
	err := d.roQueryRowContext(ctx, `
		SELECT s.id, s.repo_id, s.base_number, s.name, s.created_at, s.updated_at
		FROM forge_stacks s
		JOIN forge_stack_members sm ON sm.stack_id = s.id
		JOIN forge_merge_requests p ON p.id = sm.merge_request_id
		JOIN forge_repos r ON r.id = p.repo_id
		`+where,
		args...,
	).Scan(&stack.ID, &stack.RepoID, &stack.BaseNumber, &stack.Name, &stack.CreatedAt, &stack.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get stack for pr: %w", err)
	}

	rows, err := d.roQueryContext(ctx, `
		SELECT sm.stack_id, sm.merge_request_id, sm.position,
		       p.number, p.title, p.state, p.ci_status, p.review_decision,
		       p.is_draft, p.base_branch, p.mergeable_state
		FROM forge_stack_members sm
		JOIN forge_merge_requests p ON p.id = sm.merge_request_id
		WHERE sm.stack_id = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = p.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = p.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  )
		ORDER BY sm.position`, stack.ID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get stack members: %w", err)
	}
	defer rows.Close()

	var members []StackMemberWithPR
	for rows.Next() {
		var m StackMemberWithPR
		if err := rows.Scan(
			&m.StackID, &m.MergeRequestID, &m.Position,
			&m.Number, &m.Title, &m.State, &m.CIStatus, &m.ReviewDecision,
			&m.IsDraft, &m.BaseBranch, &m.MergeableState,
		); err != nil {
			return nil, nil, fmt.Errorf("scan stack member: %w", err)
		}
		m.Position = len(members) + 1
		members = append(members, m)
	}
	return &stack, members, rows.Err()
}

// ListStackPlacementsForMRs returns the visible stack position and size for
// each merge request that belongs to a stack. Members hidden as removed
// upstream are excluded and the remaining members are renumbered contiguously,
// matching GetStackForPR. The IDs are bound once as a JSON array so the pull
// list size is not limited by SQLite's bind-parameter ceiling.
func (d *DB) ListStackPlacementsForMRs(ctx context.Context, mrIDs []int64) (map[int64]StackPlacement, error) {
	placements := make(map[int64]StackPlacement)
	if len(mrIDs) == 0 {
		return placements, nil
	}

	payload, err := json.Marshal(mrIDs)
	if err != nil {
		return nil, fmt.Errorf("encode stack placement ids: %w", err)
	}

	rows, err := d.roQueryContext(ctx, `
		WITH requested AS (
		    SELECT CAST(value AS INTEGER) AS merge_request_id FROM json_each(?)
		),
		visible AS (
		    SELECT sm.stack_id, sm.merge_request_id,
		           ROW_NUMBER() OVER (PARTITION BY sm.stack_id ORDER BY sm.position) AS position,
		           COUNT(*) OVER (PARTITION BY sm.stack_id) AS size
		    FROM forge_stack_members sm
		    JOIN forge_merge_requests p ON p.id = sm.merge_request_id
		    WHERE sm.stack_id IN (
		        SELECT stack_id FROM forge_stack_members
		        WHERE merge_request_id IN (SELECT merge_request_id FROM requested)
		    )
		    AND NOT EXISTS (
		        SELECT 1 FROM forge_archive_items ai
		        WHERE ai.repo_id = p.repo_id
		          AND ai.item_type = 'merge_request'
		          AND ai.item_number = p.number
		          AND ai.lifecycle_state = 'removed_upstream'
		    )
		)
		SELECT merge_request_id, position, size
		FROM visible
		WHERE merge_request_id IN (SELECT merge_request_id FROM requested)`,
		string(payload),
	)
	if err != nil {
		return nil, fmt.Errorf("list stack placements: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var placement StackPlacement
		if err := rows.Scan(&id, &placement.Position, &placement.Size); err != nil {
			return nil, fmt.Errorf("scan stack placement: %w", err)
		}
		placements[id] = placement
	}
	return placements, rows.Err()
}

// ListMRsBlockedByStackConflicts returns merge request IDs whose stack has an
// earlier non-merged dirty member. It is used by API response assembly to
// surface the stack-root conflict without mutating the provider-sourced PR row.
func (d *DB) ListMRsBlockedByStackConflicts(ctx context.Context, mrIDs []int64) (map[int64]bool, error) {
	blocked := make(map[int64]bool)
	if len(mrIDs) == 0 {
		return blocked, nil
	}

	args := make([]any, len(mrIDs))
	for i, id := range mrIDs {
		args[i] = id
	}

	rows, err := d.roQueryContext(ctx, `
		SELECT DISTINCT target.merge_request_id
		FROM forge_stack_members target
		JOIN forge_merge_requests target_pr ON target_pr.id = target.merge_request_id
		JOIN forge_stack_members blocker
		  ON blocker.stack_id = target.stack_id
		 AND blocker.position < target.position
		JOIN forge_merge_requests blocker_pr ON blocker_pr.id = blocker.merge_request_id
		WHERE target.merge_request_id IN (`+sqlPlaceholders(len(mrIDs))+`)
		  AND target_pr.state = 'open'
		  AND blocker_pr.state != 'merged'
		  AND blocker_pr.mergeable_state = 'dirty'
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = target_pr.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = target_pr.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM forge_archive_items ai
		      WHERE ai.repo_id = blocker_pr.repo_id
		        AND ai.item_type = 'merge_request'
		        AND ai.item_number = blocker_pr.number
		        AND ai.lifecycle_state = 'removed_upstream'
		  )`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("list mrs blocked by stack conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stack conflict blocker: %w", err)
		}
		blocked[id] = true
	}
	return blocked, rows.Err()
}
