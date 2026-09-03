package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrWorkspaceSetupInProgress = errors.New("workspace setup is still in progress")

// listSearchCondition returns a SQL condition and args for a free-text search.
// Unquoted whitespace-separated terms are ANDed. Within each term, the title
// is searched as "#{number} {title}" so substring queries can match the
// number, the title, or both at once (e.g. "278" hits "#278 fix bug").
// Author and repository path/name are matched separately. Labels are matched
// by name for list aliases with item-label join tables. The alias is the table
// alias used in the surrounding query (e.g. "p" for merge requests, "i" for
// issues), and the repository table must be joined as alias "r".
func listSearchCondition(alias, search string) (string, []any) {
	terms := listSearchTerms(search)
	if len(terms) == 0 {
		return "", nil
	}
	labelCondition := ""
	switch alias {
	case "p":
		labelCondition = fmt.Sprintf(
			` OR EXISTS (
				SELECT 1
				FROM forge_merge_request_labels mrl
				JOIN forge_labels l ON l.id = mrl.label_id
				WHERE mrl.merge_request_id = %s.id AND l.name LIKE ?
			)`,
			alias,
		)
	case "i":
		labelCondition = fmt.Sprintf(
			` OR EXISTS (
				SELECT 1
				FROM forge_issue_labels il
				JOIN forge_labels l ON l.id = il.label_id
				WHERE il.issue_id = %s.id AND l.name LIKE ?
			)`,
			alias,
		)
	}
	termCondition := fmt.Sprintf(
		"(('#' || %s.number || ' ' || %s.title) LIKE ? OR %s.author LIKE ? OR r.repo_path LIKE ? OR r.owner LIKE ? OR r.name LIKE ?%s)",
		alias, alias, alias, labelCondition,
	)
	conds := make([]string, 0, len(terms))
	args := make([]any, 0, len(terms)*6)
	for _, term := range terms {
		conds = append(conds, termCondition)
		like := "%" + term + "%"
		args = append(args, like, like, like, like, like)
		if labelCondition != "" {
			args = append(args, like)
		}
	}
	return "(" + strings.Join(conds, " AND ") + ")", args
}

func listSearchTerms(search string) []string {
	var terms []string
	var b strings.Builder
	var quote rune

	flush := func() {
		term := strings.TrimSpace(b.String())
		if term != "" {
			terms = append(terms, term)
		}
		b.Reset()
	}

	for _, r := range search {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
		case r == '"' || r == '\'':
			if b.Len() == 0 {
				quote = r
				continue
			}
			b.WriteRune(r)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return terms
}

func appendLimitOffset(query string, args *[]any, limit, offset int) string {
	if limit > 0 {
		query += " LIMIT ?"
		*args = append(*args, limit)
		if offset > 0 {
			query += " OFFSET ?"
			*args = append(*args, offset)
		}
		return query
	}
	if offset > 0 {
		query += " LIMIT -1 OFFSET ?"
		*args = append(*args, offset)
	}
	return query
}

func workspaceActivityCTE(
	alias string,
	overrides []ItemActivityOverride,
) (prefix, join, order string, args []any, err error) {
	type key struct {
		repoID int64
		number int
	}
	deduped := make([]ItemActivityOverride, 0, len(overrides))
	indexes := make(map[key]int, len(overrides))
	for _, override := range overrides {
		if override.RepoID == 0 || override.ItemNumber <= 0 || override.ActivityAt.IsZero() {
			continue
		}
		k := key{repoID: override.RepoID, number: override.ItemNumber}
		if i, ok := indexes[k]; ok {
			if override.ActivityAt.After(deduped[i].ActivityAt) {
				deduped[i] = override
			}
			continue
		}
		indexes[k] = len(deduped)
		deduped = append(deduped, override)
	}
	if len(deduped) == 0 {
		return "", "", alias + ".last_activity_at", nil, nil
	}
	type workspaceActivityJSONRow struct {
		RepoID     int64  `json:"repo_id"`
		ItemNumber int    `json:"item_number"`
		ActivityAt string `json:"activity_at"`
	}
	rows := make([]workspaceActivityJSONRow, 0, len(deduped))
	for _, override := range deduped {
		rows = append(rows, workspaceActivityJSONRow{
			RepoID: override.RepoID, ItemNumber: override.ItemNumber,
			// modernc's default time.Time binding uses Time.String. Matching it
			// keeps effective-activity comparisons identical to the former
			// one-bind-per-field VALUES relation.
			ActivityAt: override.ActivityAt.UTC().String(),
		})
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("encode workspace activity overrides: %w", err)
	}
	prefix = `WITH workspace_activity(repo_id, item_number, activity_at) AS (
		SELECT CAST(json_extract(value, '$.repo_id') AS INTEGER),
		       CAST(json_extract(value, '$.item_number') AS INTEGER),
		       json_extract(value, '$.activity_at')
		FROM json_each(?)
	)`
	join = "LEFT JOIN workspace_activity wa ON wa.repo_id = " + alias +
		".repo_id AND wa.item_number = " + alias + ".number"
	order = "CASE WHEN wa.activity_at > " + alias + ".last_activity_at " +
		"THEN wa.activity_at ELSE " + alias + ".last_activity_at END"
	return prefix, join, order, []any{string(payload)}, nil
}

func sqlPlaceholders(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func canonicalRepoIdentifier(host, owner, name string) (string, string, string) {
	if host == "" {
		host = "github.com"
	}
	return strings.ToLower(host), strings.ToLower(owner), strings.ToLower(name)
}

func canonicalRepoLookupIdentifier(host, owner, name string) (string, string, string) {
	if host == "" {
		host = "github.com"
	}
	return strings.ToLower(strings.TrimSpace(host)),
		strings.ToLower(strings.TrimSpace(owner)),
		strings.ToLower(strings.TrimSpace(name))
}

func canonicalRepoPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		return "github"
	}
	return platform
}

func canonicalRepoPathKey(path string) string {
	parts := strings.Split(strings.Trim(path, "/ "), "/")
	kept := parts[:0]
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, strings.ToLower(trimmed))
		}
	}
	return strings.Join(kept, "/")
}

func repoFilterHostAndPathKey(filter string) (string, string) {
	filter = strings.Trim(filter, "/ ")
	if filter == "" {
		return "", ""
	}
	parts := strings.Split(filter, "/")
	if len(parts) >= 3 && strings.ContainsAny(parts[0], ".:") {
		return strings.ToLower(strings.TrimSpace(parts[0])),
			canonicalRepoPathKey(strings.Join(parts[1:], "/"))
	}
	return "", canonicalRepoPathKey(filter)
}

func repoListFilterCondition(repoAlias string, filters []RepoFilter, args *[]any) string {
	var groups []string
	for _, filter := range filters {
		var clauses []string
		if filter.PlatformRepoID != "" {
			if filter.Platform != "" {
				clauses = append(clauses, repoAlias+".platform = ?")
				*args = append(*args, strings.ToLower(strings.TrimSpace(filter.Platform)))
			}
			if filter.PlatformHost != "" {
				host, _, _ := canonicalRepoLookupIdentifier(filter.PlatformHost, "", "")
				clauses = append(clauses, repoAlias+".platform_host = ?")
				*args = append(*args, host)
			}
			clauses = append(clauses, repoAlias+".platform_repo_id = ?")
			*args = append(*args, strings.TrimSpace(filter.PlatformRepoID))
		} else if filter.RepoPath != "" {
			if filter.Platform != "" {
				clauses = append(clauses, repoAlias+".platform = ?")
				*args = append(*args, strings.ToLower(strings.TrimSpace(filter.Platform)))
			}
			if filter.PlatformHost != "" {
				host, _, _ := canonicalRepoLookupIdentifier(filter.PlatformHost, "", "")
				clauses = append(clauses, repoAlias+".platform_host = ?")
				*args = append(*args, host)
			}
			clauses = append(clauses, repoAlias+".repo_path_key = ?")
			*args = append(*args, canonicalRepoPathKey(filter.RepoPath))
		} else if filter.RepoOwner != "" && filter.RepoName != "" {
			_, owner, name := canonicalRepoLookupIdentifier(
				"", filter.RepoOwner, filter.RepoName,
			)
			if filter.Platform != "" {
				clauses = append(clauses, repoAlias+".platform = ?")
				*args = append(*args, strings.ToLower(strings.TrimSpace(filter.Platform)))
			}
			if filter.PlatformHost != "" {
				host, _, _ := canonicalRepoLookupIdentifier(filter.PlatformHost, "", "")
				clauses = append(clauses, repoAlias+".platform_host = ?")
				*args = append(*args, host)
			}
			clauses = append(clauses, repoAlias+".owner_key = ? AND "+repoAlias+".name_key = ?")
			*args = append(*args, owner, name)
		}
		if len(clauses) > 0 {
			groups = append(groups, "("+strings.Join(clauses, " AND ")+")")
		}
	}
	if len(groups) == 0 {
		return ""
	}
	return "(" + strings.Join(groups, " OR ") + ")"
}

func GitHubRepoIdentity(host, owner, name string) RepoIdentity {
	return canonicalRepoIdentity(RepoIdentity{
		Platform:     "github",
		PlatformHost: host,
		Owner:        owner,
		Name:         name,
	})
}

func canonicalRepoIdentity(identity RepoIdentity) RepoIdentity {
	identity.Platform = canonicalRepoPlatform(identity.Platform)
	identity.PlatformHost = strings.ToLower(strings.TrimSpace(identity.PlatformHost))
	if identity.PlatformHost == "" && identity.Platform == "github" {
		identity.PlatformHost = "github.com"
	}
	identity.RepoPath = strings.Trim(strings.TrimSpace(identity.RepoPath), "/")
	identity.Owner = strings.TrimSpace(identity.Owner)
	identity.Name = strings.TrimSpace(identity.Name)
	if identity.RepoPath != "" && (identity.Owner == "" || identity.Name == "") {
		if split := strings.LastIndex(identity.RepoPath, "/"); split > 0 && split < len(identity.RepoPath)-1 {
			identity.Owner = identity.RepoPath[:split]
			identity.Name = identity.RepoPath[split+1:]
		}
	}
	if identity.Platform == "github" {
		identity.Owner = strings.ToLower(identity.Owner)
		identity.Name = strings.ToLower(identity.Name)
	}
	if identity.RepoPath == "" {
		identity.RepoPath = identity.Owner + "/" + identity.Name
	} else {
		if identity.Platform == "github" {
			identity.RepoPath = strings.ToLower(identity.RepoPath)
		}
	}
	if identity.OwnerKey == "" {
		identity.OwnerKey = strings.ToLower(identity.Owner)
	} else {
		identity.OwnerKey = strings.ToLower(strings.TrimSpace(identity.OwnerKey))
	}
	if identity.NameKey == "" {
		identity.NameKey = strings.ToLower(identity.Name)
	} else {
		identity.NameKey = strings.ToLower(strings.TrimSpace(identity.NameKey))
	}
	if identity.RepoPathKey == "" {
		identity.RepoPathKey = strings.ToLower(identity.RepoPath)
	} else {
		identity.RepoPathKey = strings.ToLower(strings.TrimSpace(identity.RepoPathKey))
	}
	return identity
}

func lookupLabelIDByNameTx(ctx context.Context, tx *sql.Tx, repoID int64, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM forge_labels WHERE repo_id = ? AND name = ?`,
		repoID, name,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func labelPlatformIDTx(ctx context.Context, tx *sql.Tx, labelID int64) (sql.NullInt64, error) {
	var platformID sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT platform_id FROM forge_labels WHERE id = ?`,
		labelID,
	).Scan(&platformID)
	if err != nil {
		return sql.NullInt64{}, err
	}
	return platformID, nil
}

func mergeLabelRowAssociationsTx(ctx context.Context, tx *sql.Tx, fromLabelID, toLabelID int64) error {
	var sourceName string
	var shouldCopySourceName bool
	if err := tx.QueryRowContext(ctx, `
		SELECT source.name,
		       (source.catalog_seen_at IS NOT NULL
		           AND (target.catalog_seen_at IS NULL OR source.catalog_seen_at > target.catalog_seen_at))
		       OR (target.catalog_present = 0 AND source.updated_at > target.updated_at)
		FROM forge_labels AS source
		JOIN forge_labels AS target ON target.id = ?
		WHERE source.id = ?`,
		toLabelID, fromLabelID,
	).Scan(&sourceName, &shouldCopySourceName); err != nil {
		return fmt.Errorf("load source label metadata: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE forge_labels
		SET description = CASE
		        WHEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?) > COALESCE(catalog_seen_at, '')
		          OR (catalog_present = 0 AND (SELECT updated_at FROM forge_labels WHERE id = ?) > updated_at)
		        THEN (SELECT description FROM forge_labels WHERE id = ?)
		        ELSE description
		    END,
		    color = CASE
		        WHEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?) > COALESCE(catalog_seen_at, '')
		          OR (catalog_present = 0 AND (SELECT updated_at FROM forge_labels WHERE id = ?) > updated_at)
		        THEN (SELECT color FROM forge_labels WHERE id = ?)
		        ELSE color
		    END,
		    is_default = CASE
		        WHEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?) > COALESCE(catalog_seen_at, '')
		          OR (catalog_present = 0 AND (SELECT updated_at FROM forge_labels WHERE id = ?) > updated_at)
		        THEN (SELECT is_default FROM forge_labels WHERE id = ?)
		        ELSE is_default
		    END,
		    updated_at = CASE
		        WHEN (SELECT updated_at FROM forge_labels WHERE id = ?) > updated_at
		        THEN (SELECT updated_at FROM forge_labels WHERE id = ?)
		        ELSE updated_at
		    END,
		    catalog_present = CASE
		        WHEN catalog_present = 1 OR (SELECT catalog_present FROM forge_labels WHERE id = ?) = 1
		        THEN 1
		        ELSE catalog_present
		    END,
		    catalog_seen_at = CASE
		        WHEN catalog_seen_at IS NULL
		        THEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?)
		        WHEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?) IS NULL
		        THEN catalog_seen_at
		        WHEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?) > catalog_seen_at
		        THEN (SELECT catalog_seen_at FROM forge_labels WHERE id = ?)
		        ELSE catalog_seen_at
		    END
		WHERE id = ?`,
		fromLabelID, fromLabelID, fromLabelID,
		fromLabelID, fromLabelID, fromLabelID,
		fromLabelID, fromLabelID, fromLabelID,
		fromLabelID, fromLabelID,
		fromLabelID, fromLabelID, fromLabelID, fromLabelID, fromLabelID, toLabelID,
	); err != nil {
		return fmt.Errorf("merge label catalog metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO forge_issue_labels (issue_id, label_id)
		SELECT issue_id, ? FROM forge_issue_labels WHERE label_id = ?
		ON CONFLICT(issue_id, label_id) DO NOTHING`,
		toLabelID, fromLabelID,
	); err != nil {
		return fmt.Errorf("move issue label associations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM forge_issue_labels WHERE label_id = ?`,
		fromLabelID,
	); err != nil {
		return fmt.Errorf("delete old issue label associations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO forge_merge_request_labels (merge_request_id, label_id)
		SELECT merge_request_id, ? FROM forge_merge_request_labels WHERE label_id = ?
		ON CONFLICT(merge_request_id, label_id) DO NOTHING`,
		toLabelID, fromLabelID,
	); err != nil {
		return fmt.Errorf("move merge request label associations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM forge_merge_request_labels WHERE label_id = ?`,
		fromLabelID,
	); err != nil {
		return fmt.Errorf("delete old merge request label associations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM forge_labels WHERE id = ?`,
		fromLabelID,
	); err != nil {
		return fmt.Errorf("delete old label row: %w", err)
	}
	if shouldCopySourceName {
		if _, err := tx.ExecContext(ctx, `UPDATE forge_labels SET name = ? WHERE id = ?`, sourceName, toLabelID); err != nil {
			return fmt.Errorf("copy source label name: %w", err)
		}
	}
	return nil
}

func lookupLabelIDByPlatformIDTx(ctx context.Context, tx *sql.Tx, repoID, platformID int64) (int64, bool, error) {
	if platformID == 0 {
		return 0, false, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM forge_labels WHERE repo_id = ? AND platform_id = ?`,
		repoID, platformID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func lookupLabelIDByPlatformExternalIDTx(
	ctx context.Context,
	tx *sql.Tx,
	repoID int64,
	platformExternalID string,
) (int64, bool, error) {
	if platformExternalID == "" {
		return 0, false, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM forge_labels WHERE repo_id = ? AND platform_external_id = ?`,
		repoID, platformExternalID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func labelIDForUpsertTx(ctx context.Context, tx *sql.Tx, repoID int64, label Label) (int64, bool, error) {
	externalID, foundByExternalID, err := lookupLabelIDByPlatformExternalIDTx(ctx, tx, repoID, label.PlatformExternalID)
	if err != nil {
		return 0, false, fmt.Errorf("lookup label %s by platform external id: %w", label.Name, err)
	}
	platformID, foundByPlatform, err := lookupLabelIDByPlatformIDTx(ctx, tx, repoID, label.PlatformID)
	if err != nil {
		return 0, false, fmt.Errorf("lookup label %s by platform id: %w", label.Name, err)
	}
	nameID, foundByName, err := lookupLabelIDByNameTx(ctx, tx, repoID, label.Name)
	if err != nil {
		return 0, false, fmt.Errorf("lookup label %s by name: %w", label.Name, err)
	}
	if foundByExternalID {
		if foundByPlatform && externalID != platformID {
			return 0, false, fmt.Errorf("label %s in repo %d matches different rows by external id and platform id", label.Name, repoID)
		}
		if foundByName && externalID != nameID {
			namePlatformID, err := labelPlatformIDTx(ctx, tx, nameID)
			if err != nil {
				return 0, false, fmt.Errorf("lookup label %s platform id: %w", label.Name, err)
			}
			if !namePlatformID.Valid {
				if err := mergeLabelRowAssociationsTx(ctx, tx, nameID, externalID); err != nil {
					return 0, false, fmt.Errorf("merge stale label %s into external id row: %w", label.Name, err)
				}
			} else {
				return 0, false, fmt.Errorf("label %s in repo %d matches different rows by name and external id", label.Name, repoID)
			}
		}
		return externalID, true, nil
	}
	if foundByPlatform && foundByName && platformID != nameID {
		namePlatformID, err := labelPlatformIDTx(ctx, tx, nameID)
		if err != nil {
			return 0, false, fmt.Errorf("lookup label %s platform id: %w", label.Name, err)
		}
		if !namePlatformID.Valid {
			if err := mergeLabelRowAssociationsTx(ctx, tx, nameID, platformID); err != nil {
				return 0, false, fmt.Errorf("merge stale label %s into platform row: %w", label.Name, err)
			}
			return platformID, true, nil
		}
		return 0, false, fmt.Errorf("label %s in repo %d matches different rows by name and platform id", label.Name, repoID)
	}
	if foundByPlatform {
		return platformID, true, nil
	}
	if foundByName {
		return nameID, true, nil
	}
	return 0, false, nil
}

func repoIDForIssueTx(ctx context.Context, tx *sql.Tx, issueID int64) (int64, error) {
	var repoID int64
	err := tx.QueryRowContext(ctx,
		`SELECT repo_id FROM forge_issues WHERE id = ?`,
		issueID,
	).Scan(&repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("issue %d not found", issueID)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup issue repo: %w", err)
	}
	return repoID, nil
}

func repoIDForMergeRequestTx(ctx context.Context, tx *sql.Tx, mrID int64) (int64, error) {
	var repoID int64
	err := tx.QueryRowContext(ctx,
		`SELECT repo_id FROM forge_merge_requests WHERE id = ?`,
		mrID,
	).Scan(&repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("merge request %d not found", mrID)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup merge request repo: %w", err)
	}
	return repoID, nil
}

func upsertLabelsTx(ctx context.Context, tx *sql.Tx, repoID int64, labels []Label) (map[string]int64, error) {
	ids := make(map[string]int64, len(labels))
	for _, label := range labels {
		catalogSeenAt := label.CatalogSeenAt
		if label.CatalogPresent && catalogSeenAt == nil {
			seenAt := label.UpdatedAt.UTC()
			catalogSeenAt = &seenAt
		}
		id, found, err := labelIDForUpsertTx(ctx, tx, repoID, label)
		if err != nil {
			return nil, err
		}
		if !found {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO forge_labels (
					repo_id, platform_id, platform_external_id,
					name, description, color, is_default, updated_at,
					catalog_present, catalog_seen_at
				)
				VALUES (?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, ?)`,
				repoID, label.PlatformID, label.PlatformExternalID,
				label.Name, label.Description, label.Color, label.IsDefault, label.UpdatedAt,
				label.CatalogPresent, catalogSeenAt,
			)
			if err != nil {
				return nil, fmt.Errorf("insert label %s: %w", label.Name, err)
			}
			id, err = result.LastInsertId()
			if err != nil {
				return nil, fmt.Errorf("label insert id %s: %w", label.Name, err)
			}
		} else {
			_, err = tx.ExecContext(ctx, `
				UPDATE forge_labels
				SET platform_id = COALESCE(NULLIF(?, 0), platform_id),
				    platform_external_id = COALESCE(NULLIF(?, ''), platform_external_id),
				    name = CASE
				        WHEN (? IS NOT NULL AND (catalog_seen_at IS NULL OR ? >= catalog_seen_at)) OR (catalog_present = 0 AND ? >= updated_at) THEN ?
				        ELSE name
				    END,
				    description = CASE
				        WHEN (? IS NOT NULL AND (catalog_seen_at IS NULL OR ? >= catalog_seen_at)) OR (catalog_present = 0 AND ? >= updated_at) THEN ?
				        ELSE description
				    END,
				    color = CASE
				        WHEN (? IS NOT NULL AND (catalog_seen_at IS NULL OR ? >= catalog_seen_at)) OR (catalog_present = 0 AND ? >= updated_at) THEN ?
				        ELSE color
				    END,
				    is_default = CASE
				        WHEN (? IS NOT NULL AND (catalog_seen_at IS NULL OR ? >= catalog_seen_at)) OR (catalog_present = 0 AND ? >= updated_at) THEN ?
				        ELSE is_default
				    END,
				    updated_at = CASE
				        WHEN (? IS NOT NULL AND (catalog_seen_at IS NULL OR ? >= catalog_seen_at)) OR (catalog_present = 0 AND ? >= updated_at) THEN ?
				        ELSE updated_at
				    END,
				    catalog_present = CASE WHEN ? THEN 1 ELSE catalog_present END,
				    catalog_seen_at = CASE
				        WHEN ? IS NULL THEN catalog_seen_at
				        WHEN catalog_seen_at IS NULL OR ? > catalog_seen_at THEN ?
				        ELSE catalog_seen_at
				    END
				WHERE id = ?`,
				label.PlatformID, label.PlatformExternalID,
				catalogSeenAt, catalogSeenAt, label.UpdatedAt, label.Name,
				catalogSeenAt, catalogSeenAt, label.UpdatedAt, label.Description,
				catalogSeenAt, catalogSeenAt, label.UpdatedAt, label.Color,
				catalogSeenAt, catalogSeenAt, label.UpdatedAt, label.IsDefault,
				catalogSeenAt, catalogSeenAt, label.UpdatedAt, label.UpdatedAt,
				label.CatalogPresent, catalogSeenAt, catalogSeenAt, catalogSeenAt, id,
			)
			if err != nil {
				return nil, fmt.Errorf("update label %s: %w", label.Name, err)
			}
		}
		ids[label.Name] = id
	}
	return ids, nil
}

func replaceIssueLabelsTx(ctx context.Context, tx *sql.Tx, repoID, issueID int64, labels []Label) error {
	actualRepoID, err := repoIDForIssueTx(ctx, tx, issueID)
	if err != nil {
		return err
	}
	if actualRepoID != repoID {
		return fmt.Errorf("issue %d belongs to repo %d, not repo %d", issueID, actualRepoID, repoID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM forge_issue_labels WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("delete issue labels: %w", err)
	}
	if len(labels) == 0 {
		return nil
	}
	ids, err := upsertLabelsTx(ctx, tx, actualRepoID, labels)
	if err != nil {
		return err
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO forge_issue_labels (issue_id, label_id) VALUES (?, ?) ON CONFLICT(issue_id, label_id) DO NOTHING`,
			issueID, ids[label.Name],
		); err != nil {
			return fmt.Errorf("insert issue label %s: %w", label.Name, err)
		}
	}
	return nil
}

func replaceMergeRequestLabelsTx(ctx context.Context, tx *sql.Tx, repoID, mrID int64, labels []Label) error {
	actualRepoID, err := repoIDForMergeRequestTx(ctx, tx, mrID)
	if err != nil {
		return err
	}
	if actualRepoID != repoID {
		return fmt.Errorf("merge request %d belongs to repo %d, not repo %d", mrID, actualRepoID, repoID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM forge_merge_request_labels WHERE merge_request_id = ?`, mrID); err != nil {
		return fmt.Errorf("delete merge request labels: %w", err)
	}
	if len(labels) == 0 {
		return nil
	}
	ids, err := upsertLabelsTx(ctx, tx, actualRepoID, labels)
	if err != nil {
		return err
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO forge_merge_request_labels (merge_request_id, label_id) VALUES (?, ?) ON CONFLICT(merge_request_id, label_id) DO NOTHING`,
			mrID, ids[label.Name],
		); err != nil {
			return fmt.Errorf("insert merge request label %s: %w", label.Name, err)
		}
	}
	return nil
}

func (d *DB) UpsertLabels(ctx context.Context, repoID int64, labels []Label) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := upsertLabelsTx(ctx, tx, repoID, labels)
		return err
	})
}

// ReplaceRepoLabelCatalog replaces the selectable provider label catalog for a repo.
// Historical label rows and item-label joins are preserved, but labels not returned
// by the provider stop appearing in catalog results.
func (d *DB) ReplaceRepoLabelCatalog(ctx context.Context, repoID int64, labels []Label, syncedAt time.Time) error {
	syncedAt = canonicalUTCTime(syncedAt)
	return d.Tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE forge_repos
			SET label_catalog_synced_at = ?,
			    label_catalog_checked_at = CASE
			        WHEN ? >= COALESCE(label_catalog_checked_at, '') THEN ?
			        ELSE label_catalog_checked_at
			    END,
			    label_catalog_sync_error = CASE
			        WHEN ? >= COALESCE(label_catalog_checked_at, '') THEN ''
			        ELSE label_catalog_sync_error
			    END
			WHERE id = ?
			  AND (? >= COALESCE(label_catalog_synced_at, ''))`,
			syncedAt, syncedAt, syncedAt, syncedAt, repoID, syncedAt,
		)
		if err != nil {
			return fmt.Errorf("mark label catalog synced: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check label catalog sync claim: %w", err)
		}
		if rowsAffected == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE forge_labels SET catalog_present = 0 WHERE repo_id = ?`,
			repoID,
		); err != nil {
			return fmt.Errorf("clear label catalog: %w", err)
		}
		for i := range labels {
			labels[i].CatalogPresent = true
			labels[i].CatalogSeenAt = &syncedAt
			if labels[i].UpdatedAt.IsZero() {
				labels[i].UpdatedAt = syncedAt
			}
		}
		if _, err := upsertLabelsTx(ctx, tx, repoID, labels); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) ListRepoLabelCatalog(ctx context.Context, repoID int64) ([]Label, LabelCatalogFreshness, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, repo_id, COALESCE(platform_id, 0), platform_external_id,
		       name, description, color, is_default, updated_at,
		       catalog_present, catalog_seen_at
		FROM forge_labels
		WHERE repo_id = ? AND catalog_present = 1
		ORDER BY lower(name), name`,
		repoID,
	)
	if err != nil {
		return nil, LabelCatalogFreshness{}, fmt.Errorf("list repo label catalog: %w", err)
	}
	defer rows.Close()

	labels := []Label{}
	for rows.Next() {
		var label Label
		var seenAt sql.NullTime
		if err := rows.Scan(
			&label.ID, &label.RepoID, &label.PlatformID, &label.PlatformExternalID,
			&label.Name, &label.Description, &label.Color, &label.IsDefault,
			&label.UpdatedAt, &label.CatalogPresent, &seenAt,
		); err != nil {
			return nil, LabelCatalogFreshness{}, fmt.Errorf("scan repo label catalog: %w", err)
		}
		label.UpdatedAt = label.UpdatedAt.UTC()
		if seenAt.Valid {
			seen := seenAt.Time.UTC()
			label.CatalogSeenAt = &seen
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return nil, LabelCatalogFreshness{}, fmt.Errorf("iterate repo label catalog: %w", err)
	}
	freshness, err := d.GetRepoLabelCatalogFreshness(ctx, repoID)
	if err != nil {
		return nil, LabelCatalogFreshness{}, err
	}
	return labels, freshness, nil
}

func (d *DB) GetRepoLabelCatalogFreshness(ctx context.Context, repoID int64) (LabelCatalogFreshness, error) {
	var freshness LabelCatalogFreshness
	err := d.roQueryRowContext(ctx, `
		SELECT label_catalog_synced_at, label_catalog_checked_at, label_catalog_sync_error
		FROM forge_repos
		WHERE id = ?`, repoID,
	).Scan(&freshness.SyncedAt, &freshness.CheckedAt, &freshness.SyncError)
	if err != nil {
		return LabelCatalogFreshness{}, fmt.Errorf("get label catalog freshness: %w", err)
	}
	if freshness.SyncedAt != nil {
		t := freshness.SyncedAt.UTC()
		freshness.SyncedAt = &t
	}
	if freshness.CheckedAt != nil {
		t := freshness.CheckedAt.UTC()
		freshness.CheckedAt = &t
	}
	return freshness, nil
}

func (d *DB) UpdateRepoLabelCatalogCheck(ctx context.Context, repoID int64, checkedAt time.Time, syncErr string) error {
	checkedAt = canonicalUTCTime(checkedAt)
	_, err := d.execContext(ctx, `
		UPDATE forge_repos
		SET label_catalog_checked_at = ?, label_catalog_sync_error = ?
		WHERE id = ?
		  AND (? >= COALESCE(label_catalog_checked_at, ''))`,
		checkedAt, syncErr, repoID, checkedAt,
	)
	if err != nil {
		return fmt.Errorf("update label catalog check: %w", err)
	}
	return nil
}

func (d *DB) MarkRepoLabelCatalogSynced(ctx context.Context, repoID int64, syncedAt time.Time) error {
	syncedAt = canonicalUTCTime(syncedAt)
	_, err := d.execContext(ctx, `
		UPDATE forge_repos
		SET label_catalog_synced_at = CASE
		        WHEN ? >= COALESCE(label_catalog_synced_at, '') THEN ?
		        ELSE label_catalog_synced_at
		    END,
		    label_catalog_checked_at = CASE
		        WHEN ? >= COALESCE(label_catalog_checked_at, '') THEN ?
		        ELSE label_catalog_checked_at
		    END,
		    label_catalog_sync_error = CASE
		        WHEN ? >= COALESCE(label_catalog_checked_at, '') THEN ''
		        ELSE label_catalog_sync_error
		    END
		WHERE id = ?`,
		syncedAt, syncedAt, syncedAt, syncedAt, syncedAt, repoID,
	)
	if err != nil {
		return fmt.Errorf("mark label catalog synced: %w", err)
	}
	return nil
}

func (d *DB) ReplaceIssueLabels(ctx context.Context, repoID, issueID int64, labels []Label) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return replaceIssueLabelsTx(ctx, tx, repoID, issueID, labels)
	})
}

func (d *DB) ReplaceMergeRequestLabels(ctx context.Context, repoID, mrID int64, labels []Label) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return replaceMergeRequestLabelsTx(ctx, tx, repoID, mrID, labels)
	})
}

func (d *DB) loadLabelsForMergeRequests(ctx context.Context, ids []int64) (map[int64][]Label, error) {
	if len(ids) == 0 {
		return map[int64][]Label{}, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT ml.merge_request_id, l.id, l.repo_id, COALESCE(l.platform_id, 0),
		       l.platform_external_id, l.name, l.description, l.color, l.is_default, l.updated_at
		FROM forge_merge_request_labels ml
		JOIN forge_labels l ON l.id = ml.label_id
		WHERE ml.merge_request_id IN (%s)
		ORDER BY l.name, l.id`, sqlPlaceholders(len(ids)))
	rows, err := d.roQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query merge request labels: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Label, len(ids))
	for rows.Next() {
		var ownerID int64
		var label Label
		if err := rows.Scan(&ownerID, &label.ID, &label.RepoID, &label.PlatformID, &label.PlatformExternalID, &label.Name, &label.Description, &label.Color, &label.IsDefault, &label.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan merge request label: %w", err)
		}
		out[ownerID] = append(out[ownerID], label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate merge request labels: %w", err)
	}
	return out, nil
}

func (d *DB) loadLabelsForIssues(ctx context.Context, ids []int64) (map[int64][]Label, error) {
	if len(ids) == 0 {
		return map[int64][]Label{}, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := fmt.Sprintf(`
		SELECT il.issue_id, l.id, l.repo_id, COALESCE(l.platform_id, 0),
		       l.platform_external_id, l.name, l.description, l.color, l.is_default, l.updated_at
		FROM forge_issue_labels il
		JOIN forge_labels l ON l.id = il.label_id
		WHERE il.issue_id IN (%s)
		ORDER BY l.name, l.id`, sqlPlaceholders(len(ids)))
	rows, err := d.roQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query issue labels: %w", err)
	}
	defer rows.Close()

	out := make(map[int64][]Label, len(ids))
	for rows.Next() {
		var ownerID int64
		var label Label
		if err := rows.Scan(&ownerID, &label.ID, &label.RepoID, &label.PlatformID, &label.PlatformExternalID, &label.Name, &label.Description, &label.Color, &label.IsDefault, &label.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan issue label: %w", err)
		}
		out[ownerID] = append(out[ownerID], label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue labels: %w", err)
	}
	return out, nil
}

// PurgeOtherHosts deletes data for platform hosts other than keepHost. A
// repository referenced by a workspace survives as an inactive identity
// tombstone so purging provider data cannot detach the local checkout from its
// stable repository identity. Deletes otherwise run in FK-dependency order so
// this works on existing DBs where CASCADE may not be retrofitted.
func (d *DB) PurgeOtherHosts(ctx context.Context, keepHost string) error {
	releaseReconciliation := d.lockRepositoryReconciliationWrite()
	defer releaseReconciliation()
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		queries := []string{
			`DELETE FROM forge_starred_items WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?)`,
			`DELETE FROM forge_mr_worktree_links WHERE merge_request_id IN (SELECT id FROM forge_merge_requests WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?))`,
			`DELETE FROM forge_item_workflow_state WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?)`,
			`DELETE FROM forge_mr_events WHERE merge_request_id IN (SELECT id FROM forge_merge_requests WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?))`,
			`DELETE FROM forge_merge_requests WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?)`,
			`DELETE FROM forge_issue_events WHERE issue_id IN (SELECT id FROM forge_issues WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?))`,
			`DELETE FROM forge_issues WHERE repo_id IN (SELECT id FROM forge_repos WHERE platform_host != ?)`,
			`UPDATE forge_repo_routes SET is_current = 0
			 WHERE repo_id IN (
				 SELECT id FROM forge_repos WHERE platform_host != ?
				   AND id IN (SELECT repo_id FROM forge_workspaces WHERE repo_id IS NOT NULL)
			 )`,
			`UPDATE forge_repos SET lifecycle_state = 'inactive'
			 WHERE platform_host != ?
			   AND id IN (SELECT repo_id FROM forge_workspaces WHERE repo_id IS NOT NULL)`,
			`DELETE FROM forge_repos WHERE platform_host != ?
			   AND id NOT IN (SELECT repo_id FROM forge_workspaces WHERE repo_id IS NOT NULL)`,
			`DELETE FROM forge_rate_limits WHERE platform_host != ?`,
		}
		for _, q := range queries {
			if _, err := tx.ExecContext(ctx, q, keepHost); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Repos ---

// UpsertRepo inserts a repo identity if it does not exist, then returns its ID.
// Callers pass cached identities without provider verification, so a known
// provider ID resolves read-only; routes move only through provider-verified
// reconciliation.
func (d *DB) UpsertRepo(ctx context.Context, identity RepoIdentity) (int64, error) {
	identity.PlatformRepoID = strings.TrimSpace(identity.PlatformRepoID)
	identity = canonicalRepoIdentity(identity)
	if identity.PlatformRepoID != "" {
		return d.upsertRepoByProviderID(ctx, identity)
	}
	if identity.PlatformHost == "" || identity.Owner == "" || identity.Name == "" {
		return 0, errors.New(
			"upsert repo requires platform, host, owner, and name",
		)
	}
	releaseReconciliation := d.lockRepositoryReconciliationWrite()
	defer releaseReconciliation()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = upsertRepoIdentityTx(ctx, tx, identity)
		return err
	})
	return id, err
}

func upsertRepoIdentityTx(ctx context.Context, tx *sql.Tx, identity RepoIdentity) (int64, error) {
	identity = canonicalRepoIdentity(identity)
	if identity.PlatformRepoID != "" {
		return 0, errors.New(
			"route-only repository upsert cannot assign a provider id",
		)
	}
	if id, found, err := currentRepositoryIDByRouteTx(ctx, tx, identity); err != nil {
		return 0, err
	} else if found {
		return id, nil
	}
	var legacyID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM forge_repos
		WHERE platform = ?
		  AND platform_host = ?
		  AND platform_repo_id = ''
		  AND repo_path_key = ?
		ORDER BY id
		LIMIT 1`,
		identity.Platform,
		identity.PlatformHost,
		identity.RepoPathKey,
	).Scan(&legacyID)
	if err == nil {
		return legacyID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup legacy repository route: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO forge_repos (
		     platform, platform_host, platform_repo_id,
		     owner, name, repo_path,
		     owner_key, name_key, repo_path_key,
		     lifecycle_state
		 )
		 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, 'inactive')`,
		identity.Platform, identity.PlatformHost,
		identity.Owner, identity.Name, identity.RepoPath,
		identity.OwnerKey, identity.NameKey, identity.RepoPathKey,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert repo: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read legacy repository id: %w", err)
	}
	observedAt := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO forge_repo_routes (
			repo_id, platform, platform_host,
			owner, name, repo_path, owner_key, name_key, repo_path_key,
			is_current, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		id,
		identity.Platform,
		identity.PlatformHost,
		identity.Owner,
		identity.Name,
		identity.RepoPath,
		identity.OwnerKey,
		identity.NameKey,
		identity.RepoPathKey,
		observedAt,
		observedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("record legacy repository route: %w", err)
	}
	return id, nil
}

// UpsertRepoByProviderID resolves a cached identity by its stable provider
// ID, creating the catalog entry only when the ID is unknown. It never moves
// routes: cached writes carry no provider verification, and a delayed one
// must not reclaim a route another repository has since taken.
func (d *DB) UpsertRepoByProviderID(ctx context.Context, identity RepoIdentity) (int64, error) {
	identity.PlatformRepoID = strings.TrimSpace(identity.PlatformRepoID)
	identity = canonicalRepoIdentity(identity)
	return d.upsertRepoByProviderID(ctx, identity)
}

func (d *DB) upsertRepoByProviderID(ctx context.Context, identity RepoIdentity) (int64, error) {
	// Deliberately lock-free: callers like the MR snapshot upsert already
	// hold the reconciliation read lock, and nested acquisition deadlocks
	// against a queued reconciliation writer holding the lock gate.
	existing, err := d.getRepositoryByProviderID(
		ctx, identity.Platform, identity.PlatformHost, identity.PlatformRepoID,
	)
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return existing.Repository.ID, nil
	}
	entry, _, err := d.ReconcileRepositoryObservation(
		ctx, identity, time.Now().UTC(),
	)
	if err != nil {
		return 0, err
	}
	return entry.Repository.ID, nil
}

func lookupRepoIdentityByIDTx(
	ctx context.Context,
	tx *sql.Tx,
	repoID int64,
) (RepoIdentity, error) {
	var identity RepoIdentity
	err := tx.QueryRowContext(ctx,
		`SELECT platform, platform_host, platform_repo_id,
		        owner, name, repo_path,
		        owner_key, name_key, repo_path_key
		 FROM forge_repos
		 WHERE id = ?`,
		repoID,
	).Scan(
		&identity.Platform, &identity.PlatformHost, &identity.PlatformRepoID,
		&identity.Owner, &identity.Name, &identity.RepoPath,
		&identity.OwnerKey, &identity.NameKey, &identity.RepoPathKey,
	)
	if err != nil {
		return RepoIdentity{}, fmt.Errorf("lookup repo identity by id: %w", err)
	}
	return canonicalRepoIdentity(identity), nil
}

func (d *DB) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := d.roQueryContext(ctx,
		`SELECT id, platform, platform_host, platform_repo_id,
		        owner, name, repo_path,
		        owner_key, name_key, repo_path_key,
		        web_url, clone_url, default_branch,
		        last_sync_started_at, last_sync_completed_at,
		        last_sync_error, allow_squash_merge, allow_merge_commit,
		        allow_rebase_merge, viewer_can_merge,
		        label_catalog_synced_at, label_catalog_checked_at,
		        label_catalog_sync_error,
		        created_at
		 FROM forge_repos
		 WHERE lifecycle_state = 'active'
		 ORDER BY owner, name, platform, platform_host`,
	)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(
			&r.ID, &r.Platform, &r.PlatformHost, &r.PlatformRepoID,
			&r.Owner, &r.Name, &r.RepoPath,
			&r.OwnerKey, &r.NameKey, &r.RepoPathKey,
			&r.WebURL, &r.CloneURL, &r.DefaultBranch,
			&r.LastSyncStartedAt, &r.LastSyncCompletedAt,
			&r.LastSyncError,
			&r.AllowSquashMerge, &r.AllowMergeCommit, &r.AllowRebaseMerge,
			&r.ViewerCanMerge,
			&r.LabelCatalogSyncedAt, &r.LabelCatalogCheckedAt,
			&r.LabelCatalogSyncError,
			&r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		normalizeRepoTimestamps(&r)
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// UpdateRepoSyncStarted records the time a sync began.
func (d *DB) UpdateRepoSyncStarted(ctx context.Context, id int64, t time.Time) error {
	t = canonicalUTCTime(t)
	_, err := d.execContext(ctx,
		`UPDATE forge_repos SET last_sync_started_at = ? WHERE id = ?`, t, id,
	)
	if err != nil {
		return fmt.Errorf("update repo sync started: %w", err)
	}
	return nil
}

// UpdateRepoSyncCompleted records the time and optional error a sync finished.
func (d *DB) UpdateRepoSyncCompleted(ctx context.Context, id int64, t time.Time, syncErr string) error {
	t = canonicalUTCTime(t)
	_, err := d.execContext(ctx,
		`UPDATE forge_repos SET last_sync_completed_at = ?, last_sync_error = ? WHERE id = ?`,
		t, syncErr, id,
	)
	if err != nil {
		return fmt.Errorf("update repo sync completed: %w", err)
	}
	return nil
}

func (d *DB) UpdateRepoProviderMetadata(
	ctx context.Context,
	repoID int64,
	metadata RepoProviderMetadata,
) error {
	metadata.PlatformRepoID = strings.TrimSpace(metadata.PlatformRepoID)
	releaseReconciliation := d.lockRepositoryReconciliationWrite()
	defer releaseReconciliation()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := d.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update repo provider metadata: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if guard := d.repositoryRouteGuard(ctx); guard != nil {
		matches, err := repositoryRouteFenceMatchesTx(
			ctx, tx, guard.identity, guard.fence,
		)
		if err != nil {
			return fmt.Errorf("update repo provider metadata: %w", err)
		}
		if !matches {
			return fmt.Errorf(
				"update repo provider metadata: %w for %s/%s",
				ErrRepositoryRouteFenceChanged,
				guard.identity.PlatformHost, guard.identity.RepoPath,
			)
		}
	}
	err = func(tx *sql.Tx) error {
		identity, err := lookupRepoIdentityByIDTx(ctx, tx, repoID)
		if err != nil {
			return err
		}
		currentProviderID := strings.TrimSpace(identity.PlatformRepoID)
		if currentProviderID != "" && metadata.PlatformRepoID != "" &&
			currentProviderID != metadata.PlatformRepoID {
			return fmt.Errorf(
				"stable provider id for repository %d cannot change from %q to %q",
				repoID, currentProviderID, metadata.PlatformRepoID,
			)
		}

		providerID := currentProviderID
		if providerID == "" {
			providerID = metadata.PlatformRepoID
		}
		if currentProviderID == "" && providerID != "" {
			identity.PlatformRepoID = providerID
			existingID, found, err := repositoryIDByProviderIDTx(
				ctx, tx, identity,
			)
			if err != nil {
				return err
			}
			if found && existingID != repoID {
				return fmt.Errorf(
					"stable provider id %q already belongs to repository %d",
					providerID, existingID,
				)
			}
			occupantID, occupied, err := currentRepositoryIDByRouteTx(
				ctx, tx, identity,
			)
			if err != nil {
				return err
			}
			if occupied && occupantID != repoID {
				return fmt.Errorf(
					"repository route %q is occupied by repository %d",
					identity.RepoPath, occupantID,
				)
			}
			if err := activateRepositoryRouteTx(
				ctx, tx, repoID, identity, time.Now().UTC(),
			); err != nil {
				return err
			}
			if err := updateRepositoryDisplayTx(ctx, tx, repoID, identity); err != nil {
				return err
			}
		}

		_, err = tx.ExecContext(ctx,
			`UPDATE forge_repos
			 SET platform_repo_id = ?,
			     web_url = ?,
			     clone_url = ?,
			     default_branch = ?
			 WHERE id = ?`,
			providerID,
			metadata.WebURL,
			metadata.CloneURL,
			metadata.DefaultBranch,
			repoID,
		)
		return err
	}(tx)
	if err != nil {
		return fmt.Errorf("update repo provider metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update repo provider metadata: commit: %w", err)
	}
	return nil
}

// GetRepoByIdentity returns the repo for the provider-qualified identity,
// or nil if not found.
func (d *DB) GetRepoByIdentity(ctx context.Context, identity RepoIdentity) (*Repo, error) {
	entry, err := d.ResolveActiveRepositoryRoute(ctx, identity)
	return repoFromCatalogEntry(entry, err)
}

// GetRepoByIdentityUnderRepositoryReconciliationRead is GetRepoByIdentity for
// callers that already hold LockRepositoryReconciliationRead — acquiring the
// lock again deadlocks behind a queued reconciliation writer.
func (d *DB) GetRepoByIdentityUnderRepositoryReconciliationRead(
	ctx context.Context, identity RepoIdentity,
) (*Repo, error) {
	entry, err := d.resolveActiveRepositoryRoute(ctx, identity)
	return repoFromCatalogEntry(entry, err)
}

func repoFromCatalogEntry(entry *RepositoryCatalogEntry, err error) (*Repo, error) {
	if err != nil {
		return nil, fmt.Errorf("get repo by identity: %w", err)
	}
	if entry == nil {
		return nil, nil
	}
	repo := entry.Repository
	return &repo, nil
}

// GetRepoByID returns the repo with the given ID, or nil if not found.
func (d *DB) GetRepoByID(ctx context.Context, id int64) (*Repo, error) {
	return d.getRepoByID(ctx, id, false)
}

// GetActiveRepoByID returns the active repo with the given ID, or nil if the
// repo does not exist or is inactive.
func (d *DB) GetActiveRepoByID(ctx context.Context, id int64) (*Repo, error) {
	return d.getRepoByID(ctx, id, true)
}

func (d *DB) getRepoByID(ctx context.Context, id int64, activeOnly bool) (*Repo, error) {
	var r Repo
	query :=
		`SELECT id, platform, platform_host, platform_repo_id,
		        owner, name, repo_path,
		        owner_key, name_key, repo_path_key,
		        web_url, clone_url, default_branch,
		        last_sync_started_at, last_sync_completed_at,
		        last_sync_error, allow_squash_merge, allow_merge_commit,
		        allow_rebase_merge, viewer_can_merge,
		        label_catalog_synced_at, label_catalog_checked_at,
		        label_catalog_sync_error,
		        created_at
		 FROM forge_repos WHERE id = ?`
	if activeOnly {
		query += ` AND lifecycle_state = 'active'`
	}
	err := d.roQueryRowContext(ctx, query, id).Scan(
		&r.ID, &r.Platform, &r.PlatformHost, &r.PlatformRepoID,
		&r.Owner, &r.Name, &r.RepoPath,
		&r.OwnerKey, &r.NameKey, &r.RepoPathKey,
		&r.WebURL, &r.CloneURL, &r.DefaultBranch,
		&r.LastSyncStartedAt, &r.LastSyncCompletedAt,
		&r.LastSyncError,
		&r.AllowSquashMerge, &r.AllowMergeCommit, &r.AllowRebaseMerge,
		&r.ViewerCanMerge,
		&r.LabelCatalogSyncedAt, &r.LabelCatalogCheckedAt,
		&r.LabelCatalogSyncError,
		&r.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by id: %w", err)
	}
	normalizeRepoTimestamps(&r)
	return &r, nil
}

func normalizeRepoTimestamps(r *Repo) {
	if r == nil {
		return
	}
	r.CreatedAt = r.CreatedAt.UTC()
	if r.LastSyncStartedAt != nil {
		t := r.LastSyncStartedAt.UTC()
		r.LastSyncStartedAt = &t
	}
	if r.LastSyncCompletedAt != nil {
		t := r.LastSyncCompletedAt.UTC()
		r.LastSyncCompletedAt = &t
	}
	if r.LabelCatalogSyncedAt != nil {
		t := r.LabelCatalogSyncedAt.UTC()
		r.LabelCatalogSyncedAt = &t
	}
	if r.LabelCatalogCheckedAt != nil {
		t := r.LabelCatalogCheckedAt.UTC()
		r.LabelCatalogCheckedAt = &t
	}
}

// UpdateRepoSettings updates the merge method settings for a repo.
func (d *DB) UpdateRepoSettings(
	ctx context.Context,
	id int64,
	allowSquash, allowMerge, allowRebase, viewerCanMerge bool,
) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_repos SET allow_squash_merge = ?, allow_merge_commit = ?, allow_rebase_merge = ?, viewer_can_merge = ? WHERE id = ?`,
		allowSquash, allowMerge, allowRebase, viewerCanMerge, id,
	)
	return err
}

// UpdateRepoMergeSettings updates the merge method settings for a repo without changing viewer permissions.
func (d *DB) UpdateRepoMergeSettings(
	ctx context.Context,
	id int64,
	allowSquash, allowMerge, allowRebase bool,
) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_repos SET allow_squash_merge = ?, allow_merge_commit = ?, allow_rebase_merge = ? WHERE id = ?`,
		allowSquash, allowMerge, allowRebase, id,
	)
	return err
}

// UpdateRepoViewerCanMerge updates the current user's merge permission for a repo without changing merge method settings.
func (d *DB) UpdateRepoViewerCanMerge(ctx context.Context, id int64, viewerCanMerge bool) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_repos SET viewer_can_merge = ? WHERE id = ?`,
		viewerCanMerge, id,
	)
	return err
}

// --- Merge Requests ---

// UpsertMergeRequest inserts or updates a merge request, returning its internal
// ID. Before writing, all timestamp fields are normalized to UTC so the raw
// SQLite DATETIME text stays comparable in SQL.
// On conflict (repo_id, number), stale snapshots are ignored wholesale.
// parseMergeRequestUserLists fills the parsed Assignees and
// RequestedReviewers slices from their JSON columns. Empty or malformed
// JSON leaves the slice nil.
func parseMergeRequestUserLists(mr *MergeRequest) {
	mr.Assignees = parseUserNamesJSON(mr.AssigneesJSON)
	mr.RequestedReviewers = parseUserNamesJSON(mr.ReviewersJSON)
}

func parseUserNamesJSON(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil
	}
	return names
}

func marshalUserNamesJSON(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	if b, err := json.Marshal(names); err == nil {
		return string(b)
	}
	return "[]"
}

// UpdateMergeRequestAssignees persists a provider-confirmed assignee set
// after a mutation so the next sync does not revert the edit.
func (d *DB) UpdateMergeRequestAssignees(ctx context.Context, repoID, mrID int64, assignees []string) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_merge_requests SET assignees_json = ? WHERE id = ? AND repo_id = ?`,
		marshalUserNamesJSON(assignees), mrID, repoID,
	)
	if err != nil {
		return fmt.Errorf("update merge request assignees: %w", err)
	}
	return nil
}

// UpdateMergeRequestReviewers persists a provider-confirmed
// requested-reviewer set after a mutation.
func (d *DB) UpdateMergeRequestReviewers(ctx context.Context, repoID, mrID int64, reviewers []string) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_merge_requests SET reviewers_json = ? WHERE id = ? AND repo_id = ?`,
		marshalUserNamesJSON(reviewers), mrID, repoID,
	)
	if err != nil {
		return fmt.Errorf("update merge request reviewers: %w", err)
	}
	return nil
}

// UpdateIssueAssignees persists a provider-confirmed assignee set on an
// issue after a mutation.
func (d *DB) UpdateIssueAssignees(ctx context.Context, repoID, issueID int64, assignees []string) error {
	_, err := d.execContext(ctx,
		`UPDATE forge_issues SET assignees_json = ? WHERE id = ? AND repo_id = ?`,
		marshalUserNamesJSON(assignees), issueID, repoID,
	)
	if err != nil {
		return fmt.Errorf("update issue assignees: %w", err)
	}
	return nil
}

func (d *DB) UpsertMergeRequest(ctx context.Context, mr *MergeRequest) (int64, error) {
	id, _, err := d.UpsertMergeRequestSnapshot(ctx, mr)
	return id, err
}

// UpsertMergeRequestSnapshotWithLabels atomically applies a provider merge
// request snapshot and its labels through the shared parent upsert core.
// Callers must skip dependent writes when the monotonic updated_at guard
// rejects the parent.
func (d *DB) UpsertMergeRequestSnapshotWithLabels(
	ctx context.Context,
	mr *MergeRequest,
) (int64, int64, bool, error) {
	release, err := d.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	defer release()
	return d.UpsertMergeRequestSnapshotWithLabelsUnderRepositoryReconciliationRead(
		ctx, mr, nil,
	)
}

// MREventMetadataComputer derives metadata_json replacements (keyed by event
// dedupe key) from a transaction's own view of a merge request's stored
// events. The parent snapshot upsert invokes it inside the snapshot
// transaction, so the computation runs against event rows no concurrent
// round can shift and its results land atomically with the snapshot.
type MREventMetadataComputer func(mergeRequestID int64, events []MREvent) map[string]string

// UpsertMergeRequestSnapshotWithLabelsUnderRepositoryReconciliationRead applies
// a parent snapshot while its caller holds LockRepositoryReconciliationRead.
// terminalEventMetadata, when non-nil, runs inside the snapshot transaction on
// the accepted round that takes the merge request out of the open state (the
// stored row was open, the incoming snapshot is not): it receives the
// transaction's view of the stored events and its returned metadata lands with
// the terminal state — together or not at all, computed from data no
// concurrent writer can change underneath it. Non-transition rounds and
// rejected snapshots never invoke it.
func (d *DB) UpsertMergeRequestSnapshotWithLabelsUnderRepositoryReconciliationRead(
	ctx context.Context,
	mr *MergeRequest,
	terminalEventMetadata MREventMetadataComputer,
) (int64, int64, bool, error) {
	release, err := d.lockMergeRequestSnapshotUnderRepositoryReconciliationRead(
		ctx, mr.RepoID, mr.Number,
	)
	if err != nil {
		return 0, 0, false, err
	}
	defer release()

	var id int64
	var revision int64
	var accepted bool
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		terminalTransition := false
		if terminalEventMetadata != nil && mr.State != MergeRequestStateOpen {
			var priorState string
			err := tx.QueryRowContext(ctx,
				`SELECT state FROM forge_merge_requests
				 WHERE repo_id = ? AND number = ?`,
				mr.RepoID, mr.Number,
			).Scan(&priorState)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("read prior mr state: %w", err)
			default:
				terminalTransition = priorState == string(MergeRequestStateOpen)
			}
		}
		var err error
		id, revision, accepted, err = commitMergeRequestParentSnapshotTx(ctx, tx, mr, mr.Labels)
		if err != nil || !accepted || !terminalTransition {
			return err
		}
		events, err := listMREvents(ctx, tx, id)
		if err != nil {
			return err
		}
		return updateMREventMetadataTx(ctx, tx, id, terminalEventMetadata(id, events))
	})
	return id, revision, accepted, err
}

// UpsertMergeRequestSnapshot reports whether the provider snapshot was
// accepted. A false result means a newer timestamp already owns the row, so
// callers must skip all downstream writes derived from the rejected snapshot.
func (d *DB) UpsertMergeRequestSnapshot(
	ctx context.Context,
	mr *MergeRequest,
) (int64, bool, error) {
	release, err := d.LockMergeRequestSnapshot(ctx, mr.RepoID, mr.Number)
	if err != nil {
		return 0, false, err
	}
	defer release()

	var id int64
	var accepted bool
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, _, accepted, err = upsertMergeRequestParentTx(ctx, tx, mr)
		return err
	})
	return id, accepted, err
}

type mergeRequestSnapshotExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func upsertMergeRequestSnapshot(
	ctx context.Context,
	executor mergeRequestSnapshotExecutor,
	mr *MergeRequest,
) (int64, int64, bool, error) {
	result, err := executor.ExecContext(ctx, `
		INSERT INTO forge_merge_requests
		    (repo_id, platform_id, platform_external_id, number, url, title, author, author_display_name,
		     state, is_draft, is_locked, body, head_branch, base_branch,
		     platform_head_sha, platform_base_sha, head_repo_clone_url,
		     additions, deletions, files_changed, merge_commit_sha, comment_count,
		     review_decision, ci_status, ci_checks_json,
		     detail_fetched_at, ci_had_pending,
		     created_at, updated_at,
		     last_activity_at, merged_at, closed_at, mergeable_state,
		     assignees_json, reviewers_json, head_repo_identity_stale,
		     snapshot_revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 1)
		ON CONFLICT(repo_id, number) DO UPDATE SET
		    platform_id          = excluded.platform_id,
		    platform_external_id = COALESCE(NULLIF(excluded.platform_external_id, ''), forge_merge_requests.platform_external_id),
		    url                  = excluded.url,
		    title                = excluded.title,
		    author               = excluded.author,
		    author_display_name  = excluded.author_display_name,
		    state                = excluded.state,
		    is_draft             = excluded.is_draft,
		    is_locked            = excluded.is_locked,
		    body                 = excluded.body,
		    head_branch          = excluded.head_branch,
		    base_branch          = excluded.base_branch,
		    platform_head_sha    = excluded.platform_head_sha,
		    platform_base_sha    = excluded.platform_base_sha,
		    head_repo_clone_url  = CASE WHEN ?
		                                THEN forge_merge_requests.head_repo_clone_url
		                                ELSE excluded.head_repo_clone_url END,
		    head_repo_identity_stale = CASE
		                                   WHEN forge_merge_requests.head_repo_identity_stale
		                                        AND ?
		                                   THEN 1
		                                   ELSE 0
		                               END,
		    additions            = CASE WHEN ?
		                                THEN excluded.additions
		                                ELSE forge_merge_requests.additions END,
		    deletions            = CASE WHEN ?
		                                THEN excluded.deletions
		                                ELSE forge_merge_requests.deletions END,
		    files_changed        = COALESCE(excluded.files_changed, forge_merge_requests.files_changed),
		    merge_commit_sha     = COALESCE(NULLIF(excluded.merge_commit_sha, ''), forge_merge_requests.merge_commit_sha),
		    comment_count        = excluded.comment_count,
		    review_decision      = excluded.review_decision,
		    ci_status            = excluded.ci_status,
		    ci_checks_json       = excluded.ci_checks_json,
		    detail_fetched_at    = COALESCE(forge_merge_requests.detail_fetched_at, excluded.detail_fetched_at),
		    ci_had_pending       = forge_merge_requests.ci_had_pending,
		    updated_at           = excluded.updated_at,
		    last_activity_at     = excluded.last_activity_at,
		    merged_at            = excluded.merged_at,
		    closed_at            = excluded.closed_at,
		    mergeable_state      = excluded.mergeable_state,
		    assignees_json       = CASE WHEN excluded.assignees_json = ''
		                                THEN forge_merge_requests.assignees_json
		                                ELSE excluded.assignees_json END,
		    reviewers_json       = CASE WHEN excluded.reviewers_json = ''
		                                THEN forge_merge_requests.reviewers_json
		                                ELSE excluded.reviewers_json END,
		    snapshot_revision    = forge_merge_requests.snapshot_revision + 1
		WHERE excluded.updated_at >= forge_merge_requests.updated_at`,
		mr.RepoID, mr.PlatformID, mr.PlatformExternalID, mr.Number, mr.URL, mr.Title,
		mr.Author, mr.AuthorDisplayName,
		mr.State, mr.IsDraft, mr.IsLocked, mr.Body, mr.HeadBranch, mr.BaseBranch,
		mr.PlatformHeadSHA, mr.PlatformBaseSHA, mr.HeadRepoCloneURL,
		mr.Additions, mr.Deletions, mr.FilesChanged, mr.MergeCommitSHA,
		mr.CommentCount, mr.ReviewDecision,
		mr.CIStatus, mr.CIChecksJSON,
		mr.DetailFetchedAt, mr.CIHadPending,
		mr.CreatedAt, mr.UpdatedAt,
		mr.LastActivityAt, mr.MergedAt, mr.ClosedAt, mr.MergeableState,
		mr.AssigneesJSON, mr.ReviewersJSON,
		mr.HeadRepoCloneURLUnknown,
		mr.HeadRepoCloneURLUnknown,
		mr.AdditionsKnown, mr.DeletionsKnown,
	)
	if err != nil {
		return 0, 0, false, fmt.Errorf("upsert merge request: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, 0, false, fmt.Errorf("read upsert merge request result: %w", err)
	}
	var id, revision int64
	err = executor.QueryRowContext(ctx,
		`SELECT id, snapshot_revision FROM forge_merge_requests WHERE repo_id = ? AND number = ?`,
		mr.RepoID, mr.Number,
	).Scan(&id, &revision)
	if err != nil {
		return 0, 0, false, fmt.Errorf("get mr id after upsert: %w", err)
	}
	return id, revision, rowsAffected > 0, nil
}

// upsertMergeRequestParentTx applies a provider snapshot inside an existing
// transaction.
func upsertMergeRequestParentTx(
	ctx context.Context,
	tx *sql.Tx,
	mr *MergeRequest,
) (int64, int64, bool, error) {
	canonicalizeMergeRequestTimestamps(mr)
	id, revision, accepted, err := upsertMergeRequestSnapshot(ctx, tx, mr)
	return id, revision, accepted, err
}

// commitIssueParentSnapshotTx upserts the parent and replaces labels only when
// the incoming snapshot wins the monotonic timestamp guard.
func commitIssueParentSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	issue *Issue,
	labels []Label,
) (int64, int64, bool, error) {
	id, revision, accepted, err := upsertIssueParentTx(ctx, tx, issue)
	if err != nil || !accepted {
		return id, revision, accepted, err
	}
	if err := replaceIssueLabelsTx(ctx, tx, issue.RepoID, id, labels); err != nil {
		return 0, 0, false, err
	}
	return id, revision, true, nil
}

// commitMergeRequestParentSnapshotTx is the merge-request counterpart.
func commitMergeRequestParentSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	mr *MergeRequest,
	labels []Label,
) (int64, int64, bool, error) {
	id, revision, accepted, err := upsertMergeRequestParentTx(ctx, tx, mr)
	if err != nil || !accepted {
		return id, revision, accepted, err
	}
	if err := replaceMergeRequestLabelsTx(ctx, tx, mr.RepoID, id, labels); err != nil {
		return 0, 0, false, err
	}
	return id, revision, true, nil
}

// GetMergeRequest returns a merge request by repository identity and MR number, or nil if not found.
func (d *DB) GetMergeRequest(
	ctx context.Context,
	platform, platformHost, owner, name string,
	number int,
) (*MergeRequest, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	var mr MergeRequest
	err := d.roQueryRowContext(ctx, `
		SELECT p.id, p.snapshot_revision, p.repo_id, p.platform_id, p.platform_external_id, p.number, p.url, p.title,
		       p.author, p.author_display_name, p.state, p.is_draft, p.is_locked,
		       p.body, p.head_branch, p.base_branch,
		       p.platform_head_sha, p.platform_base_sha,
		       p.diff_head_sha, p.diff_base_sha, p.merge_base_sha,
		       p.head_repo_clone_url, p.head_repo_identity_stale,
		       p.additions, p.deletions, p.files_changed, p.merge_commit_sha,
		       p.comment_count, p.review_decision,
		       p.ci_status, p.ci_checks_json,
		       p.created_at, p.updated_at, p.last_activity_at,
		       p.merged_at, p.closed_at, p.mergeable_state,
		       p.detail_fetched_at, p.ci_had_pending,
		       p.workflow_approval_checked_at, p.workflow_approval_head_sha,
		       p.workflow_approval_required, p.workflow_approval_count,
		       p.assignees_json, p.reviewers_json,
		       COALESCE(k.status, '') AS kanban_status,
		       (s.number IS NOT NULL) AS starred
		FROM forge_merge_requests p
		JOIN forge_repos r ON r.id = p.repo_id
		LEFT JOIN forge_item_workflow_state k
		    ON k.repo_id = p.repo_id AND k.item_type = 'pr' AND k.item_number = p.number
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'pr' AND s.repo_id = p.repo_id AND s.number = p.number
		WHERE r.platform = ? AND r.platform_host = ?
		  AND r.owner_key = ? AND r.name_key = ?
		  AND r.lifecycle_state = 'active'
		  AND NOT EXISTS (
			SELECT 1 FROM forge_archive_items ai
			WHERE ai.repo_id = p.repo_id
			  AND ai.item_type = 'merge_request'
			  AND ai.item_number = p.number
			  AND ai.lifecycle_state = 'removed_upstream'
		  )
		  AND p.number = ?`,
		platform, platformHost, owner, name, number,
	).Scan(
		&mr.ID, &mr.SnapshotRevision, &mr.RepoID, &mr.PlatformID, &mr.PlatformExternalID, &mr.Number, &mr.URL, &mr.Title,
		&mr.Author, &mr.AuthorDisplayName, &mr.State, &mr.IsDraft, &mr.IsLocked,
		&mr.Body, &mr.HeadBranch, &mr.BaseBranch,
		&mr.PlatformHeadSHA, &mr.PlatformBaseSHA,
		&mr.DiffHeadSHA, &mr.DiffBaseSHA, &mr.MergeBaseSHA,
		&mr.HeadRepoCloneURL, &mr.HeadRepoIdentityStale,
		&mr.Additions, &mr.Deletions, &mr.FilesChanged, &mr.MergeCommitSHA,
		&mr.CommentCount, &mr.ReviewDecision,
		&mr.CIStatus, &mr.CIChecksJSON,
		&mr.CreatedAt, &mr.UpdatedAt, &mr.LastActivityAt,
		&mr.MergedAt, &mr.ClosedAt, &mr.MergeableState,
		&mr.DetailFetchedAt, &mr.CIHadPending,
		&mr.WorkflowApprovalCheckedAt, &mr.WorkflowApprovalHeadSHA,
		&mr.WorkflowApprovalRequired, &mr.WorkflowApprovalCount,
		&mr.AssigneesJSON, &mr.ReviewersJSON,
		&mr.KanbanStatus, &mr.Starred,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get merge request: %w", err)
	}
	parseMergeRequestUserLists(&mr)
	labelsByMR, err := d.loadLabelsForMergeRequests(ctx, []int64{mr.ID})
	if err != nil {
		return nil, fmt.Errorf("load merge request labels: %w", err)
	}
	mr.Labels = labelsByMR[mr.ID]
	return &mr, nil
}

// GetMergeRequestByRepoIDAndNumber returns a merge request by repo ID and number.
func (d *DB) GetMergeRequestByRepoIDAndNumber(ctx context.Context, repoID int64, number int) (*MergeRequest, error) {
	return d.getMergeRequestByRepoIDAndNumber(ctx, repoID, number, true)
}

// GetVisibleMergeRequestByRepoIDAndNumber returns a merge request unless its
// archive parent was removed upstream. Internal sync paths use the unfiltered
// query above so maintenance can reactivate and repair retained canonical data.
func (d *DB) GetVisibleMergeRequestByRepoIDAndNumber(
	ctx context.Context, repoID int64, number int,
) (*MergeRequest, error) {
	return d.getMergeRequestByRepoIDAndNumber(ctx, repoID, number, false)
}

func (d *DB) getMergeRequestByRepoIDAndNumber(
	ctx context.Context, repoID int64, number int, includeRemoved bool,
) (*MergeRequest, error) {
	var mr MergeRequest
	removedFilter := ""
	if !includeRemoved {
		removedFilter = ` AND NOT EXISTS (
			SELECT 1 FROM forge_archive_items ai
			WHERE ai.repo_id = p.repo_id
			  AND ai.item_type = 'merge_request'
			  AND ai.item_number = p.number
			  AND ai.lifecycle_state = 'removed_upstream'
		)`
	}
	err := d.roQueryRowContext(ctx, `
		SELECT p.id, p.snapshot_revision, p.repo_id, p.platform_id, p.platform_external_id, p.number, p.url, p.title,
		       p.author, p.author_display_name, p.state, p.is_draft, p.is_locked,
		       p.body, p.head_branch, p.base_branch,
		       p.platform_head_sha, p.platform_base_sha,
		       p.diff_head_sha, p.diff_base_sha, p.merge_base_sha,
		       p.head_repo_clone_url, p.head_repo_identity_stale,
		       p.additions, p.deletions, p.files_changed, p.merge_commit_sha,
		       p.comment_count, p.review_decision,
		       p.ci_status, p.ci_checks_json,
		       p.created_at, p.updated_at, p.last_activity_at,
		       p.merged_at, p.closed_at, p.mergeable_state,
		       p.detail_fetched_at, p.ci_had_pending,
		       p.workflow_approval_checked_at, p.workflow_approval_head_sha,
		       p.workflow_approval_required, p.workflow_approval_count,
		       p.assignees_json, p.reviewers_json,
		       COALESCE(k.status, '') AS kanban_status,
		       (s.number IS NOT NULL) AS starred
		FROM forge_merge_requests p
		LEFT JOIN forge_item_workflow_state k
		    ON k.repo_id = p.repo_id AND k.item_type = 'pr' AND k.item_number = p.number
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'pr' AND s.repo_id = p.repo_id AND s.number = p.number
		WHERE p.repo_id = ? AND p.number = ?`+removedFilter,
		repoID, number,
	).Scan(
		&mr.ID, &mr.SnapshotRevision, &mr.RepoID, &mr.PlatformID, &mr.PlatformExternalID, &mr.Number, &mr.URL, &mr.Title,
		&mr.Author, &mr.AuthorDisplayName, &mr.State, &mr.IsDraft, &mr.IsLocked,
		&mr.Body, &mr.HeadBranch, &mr.BaseBranch,
		&mr.PlatformHeadSHA, &mr.PlatformBaseSHA,
		&mr.DiffHeadSHA, &mr.DiffBaseSHA, &mr.MergeBaseSHA,
		&mr.HeadRepoCloneURL, &mr.HeadRepoIdentityStale,
		&mr.Additions, &mr.Deletions, &mr.FilesChanged, &mr.MergeCommitSHA,
		&mr.CommentCount, &mr.ReviewDecision,
		&mr.CIStatus, &mr.CIChecksJSON,
		&mr.CreatedAt, &mr.UpdatedAt, &mr.LastActivityAt,
		&mr.MergedAt, &mr.ClosedAt, &mr.MergeableState,
		&mr.DetailFetchedAt, &mr.CIHadPending,
		&mr.WorkflowApprovalCheckedAt, &mr.WorkflowApprovalHeadSHA,
		&mr.WorkflowApprovalRequired, &mr.WorkflowApprovalCount,
		&mr.AssigneesJSON, &mr.ReviewersJSON,
		&mr.KanbanStatus, &mr.Starred,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get merge request by repo id: %w", err)
	}
	parseMergeRequestUserLists(&mr)
	labelsByMR, err := d.loadLabelsForMergeRequests(ctx, []int64{mr.ID})
	if err != nil {
		return nil, fmt.Errorf("load merge request labels: %w", err)
	}
	mr.Labels = labelsByMR[mr.ID]
	return &mr, nil
}

// ListMergeRequests returns merge requests matching the given options.
// Results are ordered by last_activity_at DESC.
func (d *DB) ListMergeRequests(ctx context.Context, opts ListMergeRequestsOpts) ([]MergeRequest, error) {
	state := opts.State
	if state == "" {
		state = "open"
	}
	var conds []string
	var args []any
	conds = append(conds, `NOT EXISTS (
		SELECT 1 FROM forge_archive_items ai
		WHERE ai.repo_id = p.repo_id
		  AND ai.item_type = 'merge_request'
		  AND ai.item_number = p.number
		  AND ai.lifecycle_state = 'removed_upstream'
	)`)

	switch state {
	case "all":
		// no state filter
	case "closed":
		conds = append(conds, "(p.state IN ('closed', 'merged') OR p.is_locked)")
	default:
		conds = append(conds, "p.state = ?")
		args = append(args, state)
		if state == "open" {
			conds = append(conds, "NOT p.is_locked")
		}
	}

	if opts.RepoID != 0 {
		conds = append(conds, "p.repo_id = ?")
		args = append(args, opts.RepoID)
	} else if cond := repoListFilterCondition("r", opts.RepoFilters, &args); cond != "" {
		conds = append(conds, "r.lifecycle_state = 'active'")
		conds = append(conds, cond)
	} else if opts.RepoPath != "" {
		conds = append(conds, "r.lifecycle_state = 'active'")
		host, _, _ := canonicalRepoLookupIdentifier(opts.PlatformHost, "", "")
		if host != "" {
			conds = append(conds, "r.platform_host = ?")
			args = append(args, host)
		}
		conds = append(conds, "r.repo_path_key = ?")
		args = append(args, canonicalRepoPathKey(opts.RepoPath))
	} else if opts.RepoOwner != "" && opts.RepoName != "" {
		conds = append(conds, "r.lifecycle_state = 'active'")
		_, owner, name := canonicalRepoLookupIdentifier(
			"", opts.RepoOwner, opts.RepoName,
		)
		if opts.PlatformHost != "" {
			host, _, _ := canonicalRepoLookupIdentifier(opts.PlatformHost, "", "")
			conds = append(conds, "r.platform_host = ?")
			args = append(args, host)
		}
		conds = append(conds, "r.owner_key = ? AND r.name_key = ?")
		args = append(args, owner, name)
	} else {
		conds = append(conds, "r.lifecycle_state = 'active'")
	}
	if opts.KanbanState != "" {
		if opts.KanbanState == string(KanbanStatusNew) {
			conds = append(conds, "COALESCE(k.status, 'new') = ?")
		} else {
			conds = append(conds, "k.status = ?")
		}
		args = append(args, opts.KanbanState)
	}
	if opts.Starred {
		conds = append(conds, "s.number IS NOT NULL")
	}
	if opts.Unassigned {
		conds = append(conds, unassignedCondition("p"))
	}
	if opts.Search != "" {
		cond, condArgs := listSearchCondition("p", opts.Search)
		if cond != "" {
			conds = append(conds, cond)
			args = append(args, condArgs...)
		}
	}
	if opts.ViewerLogins != nil {
		conds = append(conds, mergeRequestInvolvementCondition("p", opts.ViewerLogins, &args))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	activityCTE, activityJoin, activityOrder, activityArgs, err := workspaceActivityCTE("p", opts.WorkspaceActivity)
	if err != nil {
		return nil, fmt.Errorf("list merge requests: %w", err)
	}
	args = append(activityArgs, args...)
	query := fmt.Sprintf(`%s
		SELECT p.id, p.snapshot_revision, p.repo_id, p.platform_id, p.platform_external_id, p.number, p.url, p.title,
		       p.author, p.author_display_name, p.state, p.is_draft, p.is_locked,
		       p.body, p.head_branch, p.base_branch,
		       p.platform_head_sha, p.platform_base_sha,
		       p.diff_head_sha, p.diff_base_sha, p.merge_base_sha,
		       p.head_repo_clone_url, p.head_repo_identity_stale,
		       p.additions, p.deletions, p.files_changed, p.merge_commit_sha,
		       p.comment_count, p.review_decision,
		       p.ci_status, p.ci_checks_json,
		       p.created_at, p.updated_at, p.last_activity_at,
		       p.merged_at, p.closed_at, p.mergeable_state,
		       p.detail_fetched_at, p.ci_had_pending,
		       p.assignees_json, p.reviewers_json,
		       COALESCE(k.status, '') AS kanban_status,
		       (s.number IS NOT NULL) AS starred
		FROM forge_merge_requests p
		JOIN forge_repos r ON r.id = p.repo_id
		LEFT JOIN forge_item_workflow_state k
		    ON k.repo_id = p.repo_id AND k.item_type = 'pr' AND k.item_number = p.number
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'pr' AND s.repo_id = p.repo_id AND s.number = p.number
		%s
		%s
		ORDER BY %s DESC, p.id DESC`, activityCTE, activityJoin, where, activityOrder)
	query = appendLimitOffset(query, &args, opts.Limit, opts.Offset)

	rows, err := d.roQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list merge requests: %w", err)
	}
	defer rows.Close()

	var mrs []MergeRequest
	var mrIDs []int64
	for rows.Next() {
		var mr MergeRequest
		if err := rows.Scan(
			&mr.ID, &mr.SnapshotRevision, &mr.RepoID, &mr.PlatformID, &mr.PlatformExternalID, &mr.Number, &mr.URL, &mr.Title,
			&mr.Author, &mr.AuthorDisplayName, &mr.State, &mr.IsDraft, &mr.IsLocked,
			&mr.Body, &mr.HeadBranch, &mr.BaseBranch,
			&mr.PlatformHeadSHA, &mr.PlatformBaseSHA,
			&mr.DiffHeadSHA, &mr.DiffBaseSHA, &mr.MergeBaseSHA,
			&mr.HeadRepoCloneURL, &mr.HeadRepoIdentityStale,
			&mr.Additions, &mr.Deletions, &mr.FilesChanged, &mr.MergeCommitSHA,
			&mr.CommentCount, &mr.ReviewDecision,
			&mr.CIStatus, &mr.CIChecksJSON,
			&mr.CreatedAt, &mr.UpdatedAt, &mr.LastActivityAt,
			&mr.MergedAt, &mr.ClosedAt, &mr.MergeableState,
			&mr.DetailFetchedAt, &mr.CIHadPending,
			&mr.AssigneesJSON, &mr.ReviewersJSON,
			&mr.KanbanStatus, &mr.Starred,
		); err != nil {
			return nil, fmt.Errorf("scan merge request: %w", err)
		}
		parseMergeRequestUserLists(&mr)
		mrs = append(mrs, mr)
		mrIDs = append(mrIDs, mr.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	labelsByMR, err := d.loadLabelsForMergeRequests(ctx, mrIDs)
	if err != nil {
		return nil, fmt.Errorf("load merge request labels: %w", err)
	}
	for i := range mrs {
		mrs[i].Labels = labelsByMR[mrs[i].ID]
	}
	return mrs, nil
}

// --- Events ---

// UpsertMREvents bulk-inserts events after normalizing CreatedAt to UTC.
// When a duplicate dedupe key is seen again, the conflict path refreshes
// mutable fields so edited events and legacy local-offset timestamps are
// repaired during normal sync.
func (d *DB) UpsertMREvents(ctx context.Context, events []MREvent) error {
	if len(events) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return upsertMREventsTx(ctx, tx, events)
	})
}

// updateMREventMetadataTx rewrites only metadata_json for the matching event
// dedupe keys. Derived-state writers (commit liveness) ride this inside the
// same revision-guarded transaction as the rest of their round's snapshot, so
// a stale round can never land metadata and newer full-row event fields are
// never overwritten by an older read.
func updateMREventMetadataTx(
	ctx context.Context,
	tx *sql.Tx,
	mergeRequestID int64,
	metadataByDedupeKey map[string]string,
) error {
	if len(metadataByDedupeKey) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		UPDATE forge_mr_events
		SET metadata_json = ?
		WHERE merge_request_id = ? AND dedupe_key = ?`)
	if err != nil {
		return fmt.Errorf("prepare update mr event metadata: %w", err)
	}
	defer stmt.Close()
	for dedupeKey, metadataJSON := range metadataByDedupeKey {
		if _, err := stmt.ExecContext(
			ctx, metadataJSON, mergeRequestID, dedupeKey,
		); err != nil {
			return fmt.Errorf(
				"update mr event metadata (dedupe_key=%s): %w",
				dedupeKey, err,
			)
		}
	}
	return nil
}

func upsertMREventsTx(ctx context.Context, tx *sql.Tx, events []MREvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO forge_mr_events
			    (merge_request_id, platform_id, platform_external_id, event_type, author, summary, body,
			     metadata_json, created_at, dedupe_key, direct_url, thread_id, position_json, resolvable, resolved)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(merge_request_id, dedupe_key) DO UPDATE SET
			    platform_id   = excluded.platform_id,
			    platform_external_id = excluded.platform_external_id,
			    event_type    = excluded.event_type,
			    author        = CASE
			        WHEN forge_mr_events.event_type = 'merged'
			         AND excluded.event_type = 'merged'
			         AND TRIM(excluded.author) = ''
			        THEN forge_mr_events.author
			        ELSE excluded.author
			    END,
			    summary       = excluded.summary,
			    body          = excluded.body,
			    metadata_json = excluded.metadata_json,
			    created_at    = excluded.created_at,
			    direct_url    = COALESCE(NULLIF(excluded.direct_url, ''), direct_url),
			    -- Events without a thread id come from provider responses
			    -- that lack discussion context (e.g. GitLab note edits);
			    -- keep the stored discussion metadata for those instead of
			    -- detaching the comment or resetting its resolution state.
			    thread_id = COALESCE(excluded.thread_id, thread_id),
			    position_json = CASE WHEN excluded.thread_id IS NULL
			        THEN position_json ELSE excluded.position_json END,
			    resolvable = CASE WHEN excluded.thread_id IS NULL
			        THEN resolvable ELSE excluded.resolvable END,
			    resolved = CASE WHEN excluded.thread_id IS NULL
			        THEN resolved ELSE excluded.resolved END`)
	if err != nil {
		return fmt.Errorf("prepare upsert mr events: %w", err)
	}
	defer stmt.Close()

	for i := range events {
		e := &events[i]
		canonicalizeMREventTimestamps(e)
		authoredMerge := e.EventType == "merged" && strings.TrimSpace(e.Author) != ""
		if e.EventType == "merged" && !authoredMerge {
			var authoredExists bool
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM forge_mr_events
					WHERE merge_request_id = ? AND event_type = 'merged'
					  AND TRIM(author) != ''
				)`, e.MergeRequestID).Scan(&authoredExists); err != nil {
				return fmt.Errorf("check authored merged event: %w", err)
			}
			if authoredExists {
				continue
			}
		}
		if authoredMerge {
			var canonicalDedupeKey string
			err := tx.QueryRowContext(ctx, `
				SELECT dedupe_key FROM forge_mr_events
				WHERE merge_request_id = ? AND event_type = 'merged'
				  AND TRIM(author) != ''
				ORDER BY id LIMIT 1`, e.MergeRequestID).Scan(&canonicalDedupeKey)
			switch {
			case err == nil:
				e.DedupeKey = canonicalDedupeKey
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("find canonical authored merged event: %w", err)
			}
		}
		if _, err := stmt.ExecContext(ctx,
			e.MergeRequestID, e.PlatformID, e.PlatformExternalID, e.EventType, e.Author, e.Summary, e.Body,
			e.MetadataJSON, e.CreatedAt, e.DedupeKey, e.DirectURL, e.ThreadID, e.PositionJSON, e.Resolvable, e.Resolved,
		); err != nil {
			return fmt.Errorf("insert mr event (dedupe_key=%s): %w", e.DedupeKey, err)
		}
		if authoredMerge {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM forge_mr_events
				WHERE merge_request_id = ? AND event_type = 'merged'
				  AND id != (
				      SELECT id FROM forge_mr_events
				      WHERE merge_request_id = ? AND event_type = 'merged'
				        AND TRIM(author) != ''
				      ORDER BY id LIMIT 1
				  )`, e.MergeRequestID, e.MergeRequestID); err != nil {
				return fmt.Errorf("delete duplicate merged events: %w", err)
			}
		}
	}
	return nil
}

// UpsertMergedActorEvent atomically enriches an existing actorless merged
// lifecycle event or inserts the authored event when no merged event exists.
// Any extra actorless merged rows are removed so one merge transition remains.
func (d *DB) UpsertMergedActorEvent(
	ctx context.Context,
	event MREvent,
) (bool, error) {
	event.Author = strings.TrimSpace(event.Author)
	if event.MergeRequestID == 0 || event.EventType != "merged" || event.Author == "" {
		return false, nil
	}
	canonicalizeMREventTimestamps(&event)
	changed := false
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var parentMergedAt sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT merged_at
			FROM forge_merge_requests
			WHERE id = ?`, event.MergeRequestID).Scan(&parentMergedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read merged-event parent timestamp: %w", err)
		}
		// A persisted merge timestamp is durable merge evidence even when a
		// legacy row's state was not normalized to merged.
		if !parentMergedAt.Valid {
			return nil
		}
		currentMergedAt, err := parseDBTime(parentMergedAt.String)
		if err != nil {
			return fmt.Errorf("parse merged-event parent time: %w", err)
		}
		event.CreatedAt = currentMergedAt

		var authoredID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM forge_mr_events
			WHERE merge_request_id = ? AND event_type = 'merged'
			  AND TRIM(author) != ''
			ORDER BY id LIMIT 1`, event.MergeRequestID).Scan(&authoredID)
		switch {
		case err == nil:
			if _, updateErr := tx.ExecContext(ctx, `
				UPDATE forge_mr_events
				SET author = ?,
				    summary = CASE WHEN TRIM(summary) = '' THEN ? ELSE summary END,
				    created_at = ?
				WHERE id = ?`, event.Author, event.Summary, event.CreatedAt, authoredID); updateErr != nil {
				return fmt.Errorf("align authored merged event: %w", updateErr)
			}
			if _, deleteErr := tx.ExecContext(ctx, `
				DELETE FROM forge_mr_events
				WHERE merge_request_id = ? AND event_type = 'merged'
				  AND id != ?`, event.MergeRequestID, authoredID); deleteErr != nil {
				return fmt.Errorf("delete duplicate merged events: %w", deleteErr)
			}
			changed = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("find authored merged event: %w", err)
		}

		var actorlessID int64
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM forge_mr_events
			WHERE merge_request_id = ? AND event_type = 'merged'
			  AND TRIM(author) = ''
			ORDER BY id LIMIT 1`, event.MergeRequestID).Scan(&actorlessID)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `
				UPDATE forge_mr_events
				SET author = ?,
				    summary = CASE WHEN TRIM(summary) = '' THEN ? ELSE summary END,
				    created_at = ?
				WHERE id = ?`, event.Author, event.Summary, event.CreatedAt, actorlessID); err != nil {
				return fmt.Errorf("enrich actorless merged event: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM forge_mr_events
				WHERE merge_request_id = ? AND event_type = 'merged'
				  AND id != ?`, event.MergeRequestID, actorlessID); err != nil {
				return fmt.Errorf("delete duplicate merged events: %w", err)
			}
			changed = true
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("find actorless merged event: %w", err)
		}

		if err := upsertMREventsTx(ctx, tx, []MREvent{event}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (d *DB) MRCommentEventExists(
	ctx context.Context,
	mrID int64,
	platformID int64,
) (bool, error) {
	var exists bool
	err := d.roQueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM forge_mr_events
			WHERE merge_request_id = ?
			  AND platform_id = ?
			  AND event_type = 'issue_comment'
		 )`,
		mrID,
		platformID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check mr comment event exists: %w", err)
	}
	return exists, nil
}

// ReplaceMRCommentEvents atomically replaces provider issue-comment events and
// their parent merge request's derived comment count.
func (d *DB) ReplaceMRCommentEvents(
	ctx context.Context,
	mrID int64,
	events []MREvent,
) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return replaceMRCommentEventsTx(ctx, tx, mrID, events)
	})
}

func replaceMRCommentEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	mrID int64,
	events []MREvent,
) error {
	query := `DELETE FROM forge_mr_events
			WHERE merge_request_id = ? AND event_type = 'issue_comment'`
	args := []any{mrID}
	if len(events) > 0 {
		query += ` AND dedupe_key NOT IN (` + sqlPlaceholders(len(events)) + `)`
		for i := range events {
			args = append(args, events[i].DedupeKey)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete missing mr comment events: %w", err)
	}
	if err := upsertMREventsTx(ctx, tx, events); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
			UPDATE forge_merge_requests
			SET comment_count = (
				SELECT COUNT(*) FROM forge_mr_events
				WHERE merge_request_id = ? AND event_type = 'issue_comment'
			)
			WHERE id = ?`, mrID, mrID); err != nil {
		return fmt.Errorf("update mr derived fields: %w", err)
	}
	return nil
}

// ListMREvents returns all events for a merge request ordered by created_at DESC.
func (d *DB) ListMREvents(ctx context.Context, mrID int64) ([]MREvent, error) {
	return listMREvents(ctx, d.roStmts, mrID)
}

type mrEventQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// listMREvents reads a merge request's events through the supplied queryer, so
// in-transaction callers (terminal liveness finalization) see the same rows
// their transaction will update.
func listMREvents(ctx context.Context, q mrEventQueryer, mrID int64) ([]MREvent, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, merge_request_id, platform_id, platform_external_id, event_type, author, summary, body,
		       metadata_json, created_at, dedupe_key, direct_url, thread_id, position_json, resolvable, resolved
		FROM forge_mr_events
		WHERE merge_request_id = ?
		ORDER BY created_at DESC`, mrID,
	)
	if err != nil {
		return nil, fmt.Errorf("list mr events: %w", err)
	}
	defer rows.Close()

	var events []MREvent
	for rows.Next() {
		var e MREvent
		var createdAtStr string
		if err := rows.Scan(
			&e.ID, &e.MergeRequestID, &e.PlatformID, &e.PlatformExternalID, &e.EventType, &e.Author, &e.Summary,
			&e.Body, &e.MetadataJSON, &createdAtStr, &e.DedupeKey, &e.DirectURL, &e.ThreadID, &e.PositionJSON, &e.Resolvable, &e.Resolved,
		); err != nil {
			return nil, fmt.Errorf("scan mr event: %w", err)
		}
		t, err := parseDBTime(createdAtStr)
		if err != nil {
			return nil, fmt.Errorf(
				"parse mr event created_at %q: %w",
				createdAtStr, err)
		}
		e.CreatedAt = t
		events = append(events, e)
	}
	return events, rows.Err()
}

// UpdateThreadResolved updates the resolved state for all events matching the
// given merge request and thread ID.
func (d *DB) UpdateThreadResolved(ctx context.Context, mrID int64, threadID string, resolved bool) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_mr_events
		SET resolved = ?
		WHERE merge_request_id = ? AND thread_id = ?`,
		resolved, mrID, threadID,
	)
	if err != nil {
		return fmt.Errorf("update thread resolved: %w", err)
	}
	return nil
}

// --- Kanban ---

func (d *DB) mergeRequestWorkflowRef(ctx context.Context, mrID int64) (int64, int, error) {
	var repoID int64
	var number int
	err := d.roQueryRowContext(ctx,
		`SELECT repo_id, number FROM forge_merge_requests WHERE id = ?`,
		mrID,
	).Scan(&repoID, &number)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup merge request workflow ref: %w", err)
	}
	return repoID, number, nil
}

// EnsureKanbanState creates a PR workflow row with status "new" if one does not exist.
func (d *DB) EnsureKanbanState(ctx context.Context, mrID int64) error {
	repoID, number, err := d.mergeRequestWorkflowRef(ctx, mrID)
	if err != nil {
		return fmt.Errorf("ensure kanban state: %w", err)
	}
	return d.EnsureItemWorkflowState(ctx, repoID, ItemTypePR, number)
}

// SetKanbanState sets the PR workflow status for a merge request (upsert).
func (d *DB) SetKanbanState(ctx context.Context, mrID int64, status string) error {
	repoID, number, err := d.mergeRequestWorkflowRef(ctx, mrID)
	if err != nil {
		return fmt.Errorf("set kanban state: %w", err)
	}
	_, err = d.SetItemWorkflowState(ctx, SetItemWorkflowStateParams{
		RepoID:     repoID,
		ItemType:   ItemTypePR,
		ItemNumber: number,
		Status:     status,
		Source:     "ui",
	})
	return err
}

// GetKanbanState returns the PR workflow state for a merge request, or nil if not found.
func (d *DB) GetKanbanState(ctx context.Context, mrID int64) (*KanbanState, error) {
	var k KanbanState
	err := d.roQueryRowContext(ctx,
		`SELECT p.id, w.status, w.updated_at
		   FROM forge_merge_requests p
		   JOIN forge_item_workflow_state w
		     ON w.repo_id = p.repo_id AND w.item_type = 'pr' AND w.item_number = p.number
		  WHERE p.id = ?`,
		mrID,
	).Scan(&k.MergeRequestID, &k.Status, &k.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get kanban state: %w", err)
	}
	return &k, nil
}

// GetPreviouslyOpenMRNumbers returns MR numbers that are open in the DB but
// not in the stillOpen set — i.e. MRs that were closed/merged since the last sync.
func (d *DB) GetPreviouslyOpenMRNumbers(
	ctx context.Context,
	repoID int64,
	stillOpen map[int]bool,
) ([]int, error) {
	rows, err := d.roQueryContext(ctx,
		`SELECT mr.number FROM forge_merge_requests mr
		 WHERE mr.repo_id = ? AND mr.state = 'open'
		   AND NOT EXISTS (
		     SELECT 1 FROM forge_archive_items ai
		     WHERE ai.repo_id = mr.repo_id
		       AND ai.item_type = 'merge_request'
		       AND ai.item_number = mr.number
		       AND ai.lifecycle_state = 'removed_upstream'
		   )`,
		repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("get previously open mrs: %w", err)
	}
	defer rows.Close()

	var closed []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan mr number: %w", err)
		}
		if !stillOpen[n] {
			closed = append(closed, n)
		}
	}
	return closed, rows.Err()
}

// MergedMRMissingActorCursor is the exclusive upper bound for one page of
// merged MRs missing an actor. MergeRequestID breaks ties between rows with
// the same merged timestamp.
type MergedMRMissingActorCursor struct {
	MergedAt       time.Time
	MergeRequestID int64
}

// MergedMRMissingActor is one merged MR whose events lack an authored
// "merged" lifecycle event. Its timestamp and stable row ID form the cursor
// for the next page.
type MergedMRMissingActor struct {
	MergeRequestID int64
	Number         int
	MergedAt       time.Time
}

// GetMergedMRNumbersMissingMergedActor returns merged MRs (merged at or after
// mergedSince and before the composite cursor) whose events lack an authored
// "merged" lifecycle event, newest merged first, capped at limit. Merges
// performed through forge itself mark the MR merged eagerly, which suppresses
// the sync-side open->closed transition that records the acting user — these
// MRs are the gap this query finds so sync can backfill the actor from the
// provider. The mergedBefore bound lets the caller sweep the whole window
// across cycles: candidates whose provider permanently reports no actor would
// otherwise occupy every newest-first batch and starve older candidates.
func (d *DB) GetMergedMRNumbersMissingMergedActor(
	ctx context.Context,
	repoID int64,
	mergedSince time.Time,
	before MergedMRMissingActorCursor,
	limit int,
) ([]MergedMRMissingActor, error) {
	rows, err := d.roQueryContext(ctx,
		`SELECT mr.id, mr.number, mr.merged_at FROM forge_merge_requests mr
		 WHERE mr.repo_id = ? AND mr.state = 'merged'
		   AND mr.merged_at >= ?
		   AND (mr.merged_at < ? OR (mr.merged_at = ? AND mr.id < ?))
		   AND NOT EXISTS (
		     SELECT 1 FROM forge_archive_items ai
		     WHERE ai.repo_id = mr.repo_id
		       AND ai.item_type = 'merge_request'
		       AND ai.item_number = mr.number
		       AND ai.lifecycle_state = 'removed_upstream'
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM forge_mr_events e
		     WHERE e.merge_request_id = mr.id
		       AND e.event_type = 'merged' AND TRIM(e.author) != ''
		   )
		 ORDER BY mr.merged_at DESC, mr.id DESC LIMIT ?`,
		repoID, mergedSince, before.MergedAt, before.MergedAt,
		before.MergeRequestID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("get merged mrs missing merged actor: %w", err)
	}
	defer rows.Close()

	var missing []MergedMRMissingActor
	for rows.Next() {
		var row MergedMRMissingActor
		if err := rows.Scan(&row.MergeRequestID, &row.Number, &row.MergedAt); err != nil {
			return nil, fmt.Errorf("scan merged mr missing actor: %w", err)
		}
		row.MergedAt = row.MergedAt.UTC()
		missing = append(missing, row)
	}
	return missing, rows.Err()
}

func (d *DB) CountOpenMergeRequestsForRepo(ctx context.Context, repoID int64) (int, error) {
	var count int
	err := d.roQueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_merge_requests
		WHERE repo_id = ? AND state = 'open'`,
		repoID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count open merge requests for repo: %w", err)
	}
	return count, nil
}

// MRDerivedFields holds child-derived summary fields. Parent activity is
// intentionally absent: only an authoritative provider parent observation may
// update merge-request activity timestamps.
type MRDerivedFields struct {
	ReviewDecision string
	CommentCount   int
}

// IssueDerivedFields holds computed fields that are refreshed after fetching issue events.
type IssueDerivedFields struct {
	CommentCount   int
	LastActivityAt time.Time
}

// UpdateMRTitleBody updates only the title, body, updated_at, and
// last_activity_at fields from an accepted provider response.
// Derived fields (CommentCount, CIStatus, etc.) are untouched.
func (d *DB) UpdateMRTitleBody(
	ctx context.Context,
	id int64,
	title, body string,
	updatedAt time.Time,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET title = ?, body = ?, updated_at = ?,
		    last_activity_at = ?
		WHERE id = ? AND updated_at <= ?`,
		title, body, updatedAt, updatedAt, id, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("update mr title/body: %w", err)
	}
	return nil
}

// UpdateIssueTitleBody updates only the title, body, updated_at, and
// last_activity_at fields on an issue. last_activity_at advances to
// MAX(existing, updatedAt) so list ordering reflects the edit.
func (d *DB) UpdateIssueTitleBody(
	ctx context.Context,
	id int64,
	title, body string,
	updatedAt time.Time,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_issues
		SET title = ?, body = ?, updated_at = ?,
		    last_activity_at = MAX(last_activity_at, ?)
		WHERE id = ? AND updated_at <= ?`,
		title, body, updatedAt, updatedAt, id, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("update issue title/body: %w", err)
	}
	return nil
}

// UpdateMRDerivedFields writes computed fields back to the merge_requests row.
func (d *DB) UpdateMRDerivedFields(
	ctx context.Context,
	repoID int64,
	number int,
	fields MRDerivedFields,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET review_decision = ?, comment_count = ?
		WHERE repo_id = ? AND number = ?`,
		fields.ReviewDecision, fields.CommentCount,
		repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update mr derived fields: %w", err)
	}
	return nil
}

// UpdateMRReviewActivity updates the review decision after a complete comment
// replacement has already derived comment_count from persisted rows.
func (d *DB) UpdateMRReviewActivity(
	ctx context.Context,
	mrID int64,
	reviewDecision string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET review_decision = ?
		WHERE id = ?`, reviewDecision, mrID)
	if err != nil {
		return fmt.Errorf("update mr review activity: %w", err)
	}
	return nil
}

// UpdateIssueDerivedFields writes computed fields back to the issues row.
func (d *DB) UpdateIssueDerivedFields(
	ctx context.Context,
	repoID int64,
	number int,
	fields IssueDerivedFields,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_issues
		SET comment_count = ?, last_activity_at = ?
		WHERE repo_id = ? AND number = ?`,
		fields.CommentCount, fields.LastActivityAt,
		repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update issue derived fields: %w", err)
	}
	return nil
}

// UpdateIssueActivity updates activity after a complete comment replacement
// has already derived comment_count from persisted rows.
func (d *DB) UpdateIssueActivity(
	ctx context.Context,
	issueID int64,
	lastActivityAt time.Time,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_issues
		SET last_activity_at = ?
		WHERE id = ?`, lastActivityAt, issueID)
	if err != nil {
		return fmt.Errorf("update issue activity: %w", err)
	}
	return nil
}

// UpdateMRCIStatus writes CI status and check runs JSON for a merge request.
func (d *DB) UpdateMRCIStatus(
	ctx context.Context,
	repoID int64,
	number int,
	ciStatus string,
	ciChecksJSON string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET ci_status = ?, ci_checks_json = ?
		WHERE repo_id = ? AND number = ?`,
		ciStatus, ciChecksJSON,
		repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update mr ci status: %w", err)
	}
	return nil
}

// UpdateMRCIStatusForHead writes CI status and check runs JSON only when the
// merge request still points at the head SHA that was refreshed.
func (d *DB) UpdateMRCIStatusForHead(
	ctx context.Context,
	repoID int64,
	number int,
	headSHA string,
	ciStatus string,
	ciChecksJSON string,
	ciHadPending bool,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET ci_status = ?, ci_checks_json = ?, ci_had_pending = ci_had_pending OR ?
		WHERE repo_id = ? AND number = ? AND platform_head_sha = ?`,
		ciStatus, ciChecksJSON, ciHadPending,
		repoID, number, headSHA,
	)
	if err != nil {
		return fmt.Errorf("update mr ci status for head: %w", err)
	}
	return nil
}

// UpdateClosedMRState atomically updates the state, timestamps, and final
// platform head/base SHAs for a MR that has transitioned to closed or merged.
// updatedAt should be the MR's UpdatedAt timestamp from the platform.
func (d *DB) UpdateClosedMRState(
	ctx context.Context,
	repoID int64,
	number int,
	state string,
	updatedAt time.Time,
	mergedAt, closedAt *time.Time,
	platformHeadSHA, platformBaseSHA string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET state = ?, merged_at = ?, closed_at = ?,
		    updated_at = ?, last_activity_at = ?,
		    platform_head_sha = ?, platform_base_sha = ?
		WHERE repo_id = ? AND number = ?`,
		state, mergedAt, closedAt, updatedAt, updatedAt,
		platformHeadSHA, platformBaseSHA, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update closed MR state: %w", err)
	}
	return nil
}

// UpdateDiffSHAs stores the locally-verified diff SHAs for a merge request.
// Called after a successful bare clone fetch and merge-base computation.
func (d *DB) UpdateDiffSHAs(ctx context.Context, repoID int64, number int, diffHead, diffBase, mergeBase string) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		 SET diff_head_sha = ?, diff_base_sha = ?, merge_base_sha = ?
		 WHERE repo_id = ? AND number = ?`,
		diffHead, diffBase, mergeBase, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update diff SHAs for MR %d: %w", number, err)
	}
	return nil
}

// UpdatePlatformSHAs stores the platform head/base SHAs for a merge
// request. Called after normalizing GitHub API data or in test setup.
func (d *DB) UpdatePlatformSHAs(
	ctx context.Context,
	repoID int64, number int,
	platformHead, platformBase string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		 SET platform_head_sha = ?, platform_base_sha = ?
		 WHERE repo_id = ? AND number = ?`,
		platformHead, platformBase, repoID, number,
	)
	if err != nil {
		return fmt.Errorf(
			"update platform SHAs for MR %d: %w", number, err)
	}
	return nil
}

// DiffSHAs holds the SHA columns needed by the diff endpoint.
type DiffSHAs struct {
	PlatformHeadSHA string
	PlatformBaseSHA string
	DiffHeadSHA     string
	DiffBaseSHA     string
	MergeBaseSHA    string
	State           string
}

// Stale reports whether the recorded diff SHAs have drifted from the
// platform SHAs. For merged PRs only head drift matters (the base
// never advances after merge). For open/closed PRs both sides can
// advance and invalidate the diff.
func (s *DiffSHAs) Stale() bool {
	if s.State == "merged" {
		return s.DiffHeadSHA != s.PlatformHeadSHA
	}
	return s.DiffHeadSHA != s.PlatformHeadSHA || s.DiffBaseSHA != s.PlatformBaseSHA
}

// GetDiffSHAs returns the diff-related SHAs for a merge request.
func (d *DB) GetDiffSHAs(
	ctx context.Context,
	platform, platformHost, owner, name string,
	number int,
) (*DiffSHAs, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	return d.getDiffSHAs(
		ctx,
		`JOIN forge_repos r ON r.id = p.repo_id
		 WHERE r.platform = ? AND r.platform_host = ?
		   AND r.owner_key = ? AND r.name_key = ?
		   AND r.lifecycle_state = 'active'
		   AND p.number = ?`,
		platform, platformHost, owner, name, number,
	)
}

// GetDiffSHAsByRepoID returns the diff-related SHAs for a merge request
// scoped to a specific repository row.
func (d *DB) GetDiffSHAsByRepoID(ctx context.Context, repoID int64, number int) (*DiffSHAs, error) {
	return d.getDiffSHAs(ctx, `WHERE p.repo_id = ? AND p.number = ?`, repoID, number)
}

func (d *DB) getDiffSHAs(ctx context.Context, where string, args ...any) (*DiffSHAs, error) {
	var s DiffSHAs
	err := d.roQueryRowContext(ctx, `
		SELECT p.platform_head_sha, p.platform_base_sha,
		       p.diff_head_sha, p.diff_base_sha, p.merge_base_sha,
		       p.state
		FROM forge_merge_requests p
		`+where,
		args...,
	).Scan(&s.PlatformHeadSHA, &s.PlatformBaseSHA,
		&s.DiffHeadSHA, &s.DiffBaseSHA, &s.MergeBaseSHA,
		&s.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get diff SHAs: %w", err)
	}
	return &s, nil
}

// UpdateMRState sets the final state and timestamps for a MR after it is closed or merged.
func (d *DB) UpdateMRState(
	ctx context.Context,
	repoID int64,
	number int,
	state string,
	mergedAt, closedAt *time.Time,
) error {
	now := time.Now().UTC()
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET state = ?, merged_at = ?, closed_at = ?,
		    updated_at = ?, last_activity_at = ?
		WHERE repo_id = ? AND number = ?`,
		state, mergedAt, closedAt, now, now, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update mr state: %w", err)
	}
	return nil
}

// UpdateMRDraftState records a provider-confirmed draft flag without
// treating the mutation response as a full merge-request snapshot.
func (d *DB) UpdateMRDraftState(
	ctx context.Context,
	repoID int64,
	number int,
	isDraft bool,
	providerUpdatedAt time.Time,
) error {
	if providerUpdatedAt.IsZero() {
		return errors.New("update mr draft state: provider updated time is required")
	}
	tx, err := d.rw.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update mr draft state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT updated_at
		FROM forge_merge_requests
		WHERE repo_id = ? AND number = ?`,
		repoID, number,
	).Scan(&updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("update mr draft state: %w", sql.ErrNoRows)
		}
		return fmt.Errorf("load mr draft state timestamps: %w", err)
	}

	providerUpdatedAt = providerUpdatedAt.UTC()
	if providerUpdatedAt.Before(updatedAt) {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
			UPDATE forge_merge_requests
			SET is_draft = ?,
			    updated_at = ?,
			    last_activity_at = ?,
			    snapshot_revision = snapshot_revision + 1
			WHERE repo_id = ? AND number = ?`,
		isDraft, providerUpdatedAt, providerUpdatedAt, repoID, number,
	); err != nil {
		return fmt.Errorf("update mr draft state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update mr draft state: %w", err)
	}
	return nil
}

// --- Issues ---

// UpsertIssue inserts or updates an issue, returning its internal ID. Before
// writing, all timestamp fields are normalized to UTC so SQL ordering/filtering
// operates on a consistent storage representation.
// On conflict (repo_id, number), stale snapshots are ignored wholesale.
func (d *DB) UpsertIssue(ctx context.Context, issue *Issue) (int64, error) {
	var id int64
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, _, _, err = upsertIssueParentTx(ctx, tx, issue)
		return err
	})
	return id, err
}

// UpsertIssueSnapshotWithLabels atomically applies a provider issue snapshot
// through the shared parent upsert core in its live (progress-optional) mode
// and reports whether it passed the monotonic updated_at guard. Callers must
// skip child writes derived from a rejected snapshot.
func (d *DB) UpsertIssueSnapshotWithLabels(
	ctx context.Context,
	issue *Issue,
) (int64, int64, bool, error) {
	var id int64
	var revision int64
	var accepted bool
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, revision, accepted, err = commitIssueParentSnapshotTx(ctx, tx, issue, issue.Labels)
		return err
	})
	return id, revision, accepted, err
}

func upsertIssueParentTx(
	ctx context.Context,
	tx *sql.Tx,
	issue *Issue,
) (int64, int64, bool, error) {
	canonicalizeIssueTimestamps(issue)
	var issueID, revision int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO forge_issues
		    (repo_id, platform_id, platform_external_id, number, url, title, author, state,
		     body, comment_count, labels_json, assignees_json, detail_fetched_at,
		     created_at, updated_at, last_activity_at, closed_at, snapshot_revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), '[]'), ?, ?, ?, ?, ?, 1)
		ON CONFLICT(repo_id, number) DO UPDATE SET
		    platform_id       = excluded.platform_id,
		    platform_external_id = COALESCE(NULLIF(excluded.platform_external_id, ''), forge_issues.platform_external_id),
		    url               = excluded.url,
		    title             = excluded.title,
		    author            = excluded.author,
		    state             = excluded.state,
		    body              = excluded.body,
		    comment_count     = excluded.comment_count,
		    labels_json       = excluded.labels_json,
		    assignees_json    = COALESCE(NULLIF(excluded.assignees_json, ''), '[]'),
		    detail_fetched_at = COALESCE(forge_issues.detail_fetched_at, excluded.detail_fetched_at),
		    updated_at        = excluded.updated_at,
		    last_activity_at  = excluded.last_activity_at,
		    closed_at         = excluded.closed_at,
		    snapshot_revision = forge_issues.snapshot_revision + 1
		WHERE excluded.updated_at >= forge_issues.updated_at
		RETURNING id, snapshot_revision`,
		issue.RepoID, issue.PlatformID, issue.PlatformExternalID, issue.Number, issue.URL,
		issue.Title, issue.Author, issue.State,
		issue.Body, issue.CommentCount, issue.LabelsJSON, issue.AssigneesJSON,
		issue.DetailFetchedAt,
		issue.CreatedAt, issue.UpdatedAt, issue.LastActivityAt, issue.ClosedAt,
	).Scan(&issueID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx,
			`SELECT id, snapshot_revision FROM forge_issues WHERE repo_id = ? AND number = ?`,
			issue.RepoID, issue.Number,
		).Scan(&issueID, &revision)
		if err != nil {
			return 0, 0, false, fmt.Errorf("get issue id after stale upsert: %w", err)
		}
		return issueID, revision, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("upsert issue parent: %w", err)
	}
	return issueID, revision, true, nil
}

// GetIssue returns an issue by repository identity and issue number, or nil if not found.
func (d *DB) GetIssue(
	ctx context.Context,
	platform, platformHost, owner, name string,
	number int,
) (*Issue, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	var issue Issue
	err := d.roQueryRowContext(ctx, `
		SELECT i.id, i.snapshot_revision, i.repo_id, i.platform_id, i.platform_external_id, i.number, i.url, i.title,
		       i.author, i.state, i.body, i.comment_count, i.labels_json, i.assignees_json,
		       i.detail_fetched_at,
		       i.created_at, i.updated_at, i.last_activity_at, i.closed_at,
		       (s.number IS NOT NULL) AS starred,
		       COALESCE(w.status, '') AS workflow_status
		FROM forge_issues i
		JOIN forge_repos r ON r.id = i.repo_id
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'issue' AND s.repo_id = i.repo_id AND s.number = i.number
		LEFT JOIN forge_item_workflow_state w
		    ON w.repo_id = i.repo_id AND w.item_type = 'issue' AND w.item_number = i.number
		WHERE r.platform = ? AND r.platform_host = ?
		  AND r.owner_key = ? AND r.name_key = ?
		  AND r.lifecycle_state = 'active'
		  AND NOT EXISTS (
			SELECT 1 FROM forge_archive_items ai
			WHERE ai.repo_id = i.repo_id
			  AND ai.item_type = 'issue'
			  AND ai.item_number = i.number
			  AND ai.lifecycle_state = 'removed_upstream'
		  )
		  AND i.number = ?`,
		platform, platformHost, owner, name, number,
	).Scan(
		&issue.ID, &issue.SnapshotRevision, &issue.RepoID, &issue.PlatformID, &issue.PlatformExternalID, &issue.Number,
		&issue.URL, &issue.Title, &issue.Author, &issue.State,
		&issue.Body, &issue.CommentCount, &issue.LabelsJSON, &issue.AssigneesJSON,
		&issue.DetailFetchedAt,
		&issue.CreatedAt, &issue.UpdatedAt, &issue.LastActivityAt,
		&issue.ClosedAt, &issue.Starred, &issue.WorkflowStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get issue: %w", err)
	}
	// Parse assignees from JSON. Best-effort: malformed JSON yields an empty
	// Assignees slice rather than failing the whole read. Writes go through
	// json.Marshal in UpsertIssue, so corruption is unexpected in practice.
	if issue.AssigneesJSON != "" && issue.AssigneesJSON != "[]" {
		_ = json.Unmarshal([]byte(issue.AssigneesJSON), &issue.Assignees)
	}
	labelsByIssue, err := d.loadLabelsForIssues(ctx, []int64{issue.ID})
	if err != nil {
		return nil, fmt.Errorf("load issue labels: %w", err)
	}
	issue.Labels = labelsByIssue[issue.ID]
	return &issue, nil
}

// GetIssueByRepoIDAndNumber returns an issue by repo ID and number.
func (d *DB) GetIssueByRepoIDAndNumber(ctx context.Context, repoID int64, number int) (*Issue, error) {
	return d.getIssueByRepoIDAndNumber(ctx, repoID, number, true)
}

// GetVisibleIssueByRepoIDAndNumber returns an issue unless its archive parent
// was removed upstream. Internal sync paths retain access for rediscovery.
func (d *DB) GetVisibleIssueByRepoIDAndNumber(
	ctx context.Context, repoID int64, number int,
) (*Issue, error) {
	return d.getIssueByRepoIDAndNumber(ctx, repoID, number, false)
}

func (d *DB) getIssueByRepoIDAndNumber(
	ctx context.Context, repoID int64, number int, includeRemoved bool,
) (*Issue, error) {
	var issue Issue
	removedFilter := ""
	if !includeRemoved {
		removedFilter = ` AND NOT EXISTS (
			SELECT 1 FROM forge_archive_items ai
			WHERE ai.repo_id = i.repo_id
			  AND ai.item_type = 'issue'
			  AND ai.item_number = i.number
			  AND ai.lifecycle_state = 'removed_upstream'
		)`
	}
	err := d.roQueryRowContext(ctx, `
		SELECT i.id, i.snapshot_revision, i.repo_id, i.platform_id, i.platform_external_id, i.number, i.url, i.title,
		       i.author, i.state, i.body, i.comment_count, i.labels_json, i.assignees_json,
		       i.detail_fetched_at,
		       i.created_at, i.updated_at, i.last_activity_at, i.closed_at,
		       (s.number IS NOT NULL) AS starred,
		       COALESCE(w.status, '') AS workflow_status
		FROM forge_issues i
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'issue' AND s.repo_id = i.repo_id AND s.number = i.number
		LEFT JOIN forge_item_workflow_state w
		    ON w.repo_id = i.repo_id AND w.item_type = 'issue' AND w.item_number = i.number
		WHERE i.repo_id = ? AND i.number = ?`+removedFilter,
		repoID, number,
	).Scan(
		&issue.ID, &issue.SnapshotRevision, &issue.RepoID, &issue.PlatformID, &issue.PlatformExternalID, &issue.Number,
		&issue.URL, &issue.Title, &issue.Author, &issue.State,
		&issue.Body, &issue.CommentCount, &issue.LabelsJSON, &issue.AssigneesJSON,
		&issue.DetailFetchedAt,
		&issue.CreatedAt, &issue.UpdatedAt, &issue.LastActivityAt,
		&issue.ClosedAt, &issue.Starred, &issue.WorkflowStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get issue by repo id: %w", err)
	}
	// Parse assignees from JSON. Best-effort: malformed JSON yields an empty
	// Assignees slice rather than failing the whole read. Writes go through
	// json.Marshal in UpsertIssue, so corruption is unexpected in practice.
	if issue.AssigneesJSON != "" && issue.AssigneesJSON != "[]" {
		_ = json.Unmarshal([]byte(issue.AssigneesJSON), &issue.Assignees)
	}
	labelsByIssue, err := d.loadLabelsForIssues(ctx, []int64{issue.ID})
	if err != nil {
		return nil, fmt.Errorf("load issue labels: %w", err)
	}
	issue.Labels = labelsByIssue[issue.ID]
	return &issue, nil
}

// ListIssues returns issues matching the given options.
func (d *DB) ListIssues(
	ctx context.Context, opts ListIssuesOpts,
) ([]Issue, error) {
	state := opts.State
	if state == "" {
		state = "open"
	}
	var conds []string
	var args []any
	conds = append(conds, `NOT EXISTS (
		SELECT 1 FROM forge_archive_items ai
		WHERE ai.repo_id = i.repo_id
		  AND ai.item_type = 'issue'
		  AND ai.item_number = i.number
		  AND ai.lifecycle_state = 'removed_upstream'
	)`)

	switch state {
	case "all":
		// no state filter
	case "closed":
		conds = append(conds, "i.state = 'closed'")
	default:
		conds = append(conds, "i.state = ?")
		args = append(args, state)
	}

	conds = append(conds, "r.lifecycle_state = 'active'")
	if cond := repoListFilterCondition("r", opts.RepoFilters, &args); cond != "" {
		conds = append(conds, cond)
	} else if opts.RepoPath != "" {
		host, _, _ := canonicalRepoLookupIdentifier(opts.PlatformHost, "", "")
		if host != "" {
			conds = append(conds, "r.platform_host = ?")
			args = append(args, host)
		}
		conds = append(conds, "r.repo_path_key = ?")
		args = append(args, canonicalRepoPathKey(opts.RepoPath))
	} else if opts.RepoOwner != "" && opts.RepoName != "" {
		host, owner, name := canonicalRepoLookupIdentifier(opts.PlatformHost, opts.RepoOwner, opts.RepoName)
		if host != "" {
			conds = append(conds, "r.platform_host = ?")
			args = append(args, host)
		}
		conds = append(conds, "r.owner_key = ? AND r.name_key = ?")
		args = append(args, owner, name)
	}
	if opts.Starred {
		conds = append(conds, "s.number IS NOT NULL")
	}
	if opts.Unassigned {
		conds = append(conds, unassignedCondition("i"))
	}
	if opts.Search != "" {
		cond, condArgs := listSearchCondition("i", opts.Search)
		if cond != "" {
			conds = append(conds, cond)
			args = append(args, condArgs...)
		}
	}
	if opts.Assignee != "" {
		// Query JSON array structurally to avoid LIKE wildcard injection.
		// COALESCE/NULLIF guards against legacy rows where assignees_json
		// may be an empty string, which would otherwise make json_each
		// raise "malformed JSON" and fail the whole query.
		conds = append(conds, `EXISTS (SELECT 1 FROM json_each(COALESCE(NULLIF(i.assignees_json, ''), '[]')) WHERE value = ?)`)
		args = append(args, opts.Assignee)
	}
	if opts.ViewerLogins != nil {
		conds = append(conds, issueInvolvementCondition("i", opts.ViewerLogins, &args))
	}
	if opts.ReferencedByPR {
		conds = append(conds, `EXISTS (
			SELECT 1 FROM forge_issue_pr_references ref
			WHERE ref.issue_id = i.id
		)`)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	activityCTE, activityJoin, activityOrder, activityArgs, err := workspaceActivityCTE("i", opts.WorkspaceActivity)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	args = append(activityArgs, args...)
	query := fmt.Sprintf(`%s
		SELECT i.id, i.snapshot_revision, i.repo_id, i.platform_id, i.platform_external_id, i.number, i.url, i.title,
		       i.author, i.state, i.body, i.comment_count, i.labels_json, i.assignees_json,
		       i.detail_fetched_at,
		       i.created_at, i.updated_at, i.last_activity_at, i.closed_at,
		       (s.number IS NOT NULL) AS starred,
		       COALESCE(w.status, '') AS workflow_status
		FROM forge_issues i
		JOIN forge_repos r ON r.id = i.repo_id
		LEFT JOIN forge_starred_items s
		    ON s.item_type = 'issue' AND s.repo_id = i.repo_id AND s.number = i.number
		LEFT JOIN forge_item_workflow_state w
		    ON w.repo_id = i.repo_id AND w.item_type = 'issue' AND w.item_number = i.number
		%s
		%s
		ORDER BY %s DESC, i.id DESC`, activityCTE, activityJoin, where, activityOrder)
	query = appendLimitOffset(query, &args, opts.Limit, opts.Offset)

	rows, err := d.roQueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()

	var issues []Issue
	var issueIDs []int64
	for rows.Next() {
		var issue Issue
		if err := rows.Scan(
			&issue.ID, &issue.SnapshotRevision, &issue.RepoID, &issue.PlatformID, &issue.PlatformExternalID, &issue.Number,
			&issue.URL, &issue.Title, &issue.Author, &issue.State,
			&issue.Body, &issue.CommentCount, &issue.LabelsJSON, &issue.AssigneesJSON,
			&issue.DetailFetchedAt,
			&issue.CreatedAt, &issue.UpdatedAt, &issue.LastActivityAt,
			&issue.ClosedAt, &issue.Starred, &issue.WorkflowStatus,
		); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		// Parse assignees from JSON. Best-effort: malformed JSON yields an empty
		// Assignees slice rather than failing the whole read. Writes go through
		// json.Marshal in UpsertIssue, so corruption is unexpected in practice.
		if issue.AssigneesJSON != "" && issue.AssigneesJSON != "[]" {
			_ = json.Unmarshal([]byte(issue.AssigneesJSON), &issue.Assignees)
		}
		issues = append(issues, issue)
		issueIDs = append(issueIDs, issue.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	labelsByIssue, err := d.loadLabelsForIssues(ctx, issueIDs)
	if err != nil {
		return nil, fmt.Errorf("load issue labels: %w", err)
	}
	for i := range issues {
		issues[i].Labels = labelsByIssue[issues[i].ID]
	}
	return issues, nil
}

// ResolveItemNumber checks whether the given number in a repo is a MR
// or issue. Returns the item type ("pr" or "issue") and whether it was
// found. MRs take precedence if both somehow exist.
func (d *DB) ResolveItemNumber(
	ctx context.Context, repoID int64, number int,
) (itemType string, found bool, err error) {
	var exists int
	err = d.roQueryRowContext(ctx,
		`SELECT 1 FROM forge_merge_requests
		 WHERE repo_id = ? AND number = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM forge_archive_items ai
		       WHERE ai.repo_id = forge_merge_requests.repo_id
		         AND ai.item_type = 'merge_request'
		         AND ai.item_number = forge_merge_requests.number
		         AND ai.lifecycle_state = 'removed_upstream'
		   )`,
		repoID, number,
	).Scan(&exists)
	if err == nil {
		return "pr", true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("check merge_requests: %w", err)
	}

	err = d.roQueryRowContext(ctx,
		`SELECT 1 FROM forge_issues
		 WHERE repo_id = ? AND number = ?
		   AND NOT EXISTS (
		       SELECT 1 FROM forge_archive_items ai
		       WHERE ai.repo_id = forge_issues.repo_id
		         AND ai.item_type = 'issue'
		         AND ai.item_number = forge_issues.number
		         AND ai.lifecycle_state = 'removed_upstream'
		   )`,
		repoID, number,
	).Scan(&exists)
	if err == nil {
		return "issue", true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("check issues: %w", err)
	}

	return "", false, nil
}

// ResolveItemNumberOfType checks whether the given typed item number exists in a repo.
func (d *DB) ResolveItemNumberOfType(
	ctx context.Context, repoID int64, number int, itemType string,
) (string, bool, error) {
	var query string
	switch itemType {
	case "pr":
		query = `SELECT 1 FROM forge_merge_requests
		         WHERE repo_id = ? AND number = ?
		           AND NOT EXISTS (
		               SELECT 1 FROM forge_archive_items ai
		               WHERE ai.repo_id = forge_merge_requests.repo_id
		                 AND ai.item_type = 'merge_request'
		                 AND ai.item_number = forge_merge_requests.number
		                 AND ai.lifecycle_state = 'removed_upstream'
		           )`
	case "issue":
		query = `SELECT 1 FROM forge_issues
		         WHERE repo_id = ? AND number = ?
		           AND NOT EXISTS (
		               SELECT 1 FROM forge_archive_items ai
		               WHERE ai.repo_id = forge_issues.repo_id
		                 AND ai.item_type = 'issue'
		                 AND ai.item_number = forge_issues.number
		                 AND ai.lifecycle_state = 'removed_upstream'
		           )`
	default:
		return "", false, fmt.Errorf("unsupported item type %q", itemType)
	}

	var exists int
	err := d.roQueryRowContext(ctx, query, repoID, number).Scan(&exists)
	if err == nil {
		return itemType, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("check %s: %w", itemType, err)
	}
	return "", false, nil
}

// UpdateIssueState sets the state and closed_at for an issue.
func (d *DB) UpdateIssueState(
	ctx context.Context,
	repoID int64,
	number int,
	state string,
	closedAt *time.Time,
) error {
	now := time.Now().UTC()
	_, err := d.execContext(ctx, `
		UPDATE forge_issues SET state = ?, closed_at = ?,
		    updated_at = ?, last_activity_at = ?
		WHERE repo_id = ? AND number = ?`,
		state, closedAt, now, now, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update issue state: %w", err)
	}
	return nil
}

// GetPreviouslyOpenIssueNumbers returns issue numbers that are open in the DB
// but not in the stillOpen set.
func (d *DB) GetPreviouslyOpenIssueNumbers(
	ctx context.Context,
	repoID int64,
	stillOpen map[int]bool,
) ([]int, error) {
	rows, err := d.roQueryContext(ctx,
		`SELECT i.number FROM forge_issues i
		 WHERE i.repo_id = ? AND i.state = 'open'
		   AND NOT EXISTS (
		     SELECT 1 FROM forge_archive_items ai
		     WHERE ai.repo_id = i.repo_id
		       AND ai.item_type = 'issue'
		       AND ai.item_number = i.number
		       AND ai.lifecycle_state = 'removed_upstream'
		   )`,
		repoID,
	)
	if err != nil {
		return nil, fmt.Errorf("get previously open issues: %w", err)
	}
	defer rows.Close()

	var closed []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan issue number: %w", err)
		}
		if !stillOpen[n] {
			closed = append(closed, n)
		}
	}
	return closed, rows.Err()
}

func (d *DB) CountOpenIssuesForRepo(ctx context.Context, repoID int64) (int, error) {
	var count int
	err := d.roQueryRowContext(ctx,
		`SELECT COUNT(*) FROM forge_issues
		WHERE repo_id = ? AND state = 'open'`,
		repoID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count open issues for repo: %w", err)
	}
	return count, nil
}

func (d *DB) GetHTTPEtag(
	ctx context.Context,
	platform, platformHost, owner, name, resourceType string,
	resourceNumber int,
) (string, error) {
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	var etag string
	err := d.roQueryRowContext(ctx,
		`SELECT etag FROM forge_http_etags
		WHERE platform = ?
		  AND platform_host = ?
		  AND owner_key = ?
		  AND name_key = ?
		  AND resource_type = ?
		  AND resource_number = ?`,
		platform, platformHost, owner, name, resourceType, resourceNumber,
	).Scan(&etag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get http etag: %w", err)
	}
	return etag, nil
}

func (d *DB) UpsertHTTPEtag(
	ctx context.Context,
	platform, platformHost, owner, name, resourceType string,
	resourceNumber int,
	etag string,
) error {
	if etag == "" {
		return nil
	}
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return upsertHTTPEtagTx(
			ctx, tx, platform, platformHost, owner, name,
			resourceType, resourceNumber, etag,
		)
	})
}

func (d *DB) UpsertHTTPEtagIfRouteFence(
	ctx context.Context,
	identity RepoIdentity,
	fence RepositoryRouteFence,
	resourceType string,
	resourceNumber int,
	etag string,
) (bool, error) {
	if etag == "" {
		return true, nil
	}
	identity = canonicalRepoIdentity(identity)
	guarded := d.WithRepositoryRouteFence(ctx, identity, fence)
	err := d.Tx(guarded, func(tx *sql.Tx) error {
		return upsertHTTPEtagTx(
			guarded, tx, identity.Platform, identity.PlatformHost,
			identity.OwnerKey, identity.NameKey,
			resourceType, resourceNumber, etag,
		)
	})
	if errors.Is(err, ErrRepositoryRouteFenceChanged) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("conditionally upsert http etag: %w", err)
	}
	return true, nil
}

func upsertHTTPEtagTx(
	ctx context.Context,
	tx *sql.Tx,
	platform, platformHost, owner, name, resourceType string,
	resourceNumber int,
	etag string,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO forge_http_etags (
			platform, platform_host, owner_key, name_key,
			resource_type, resource_number, etag, fetched_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT (
			platform, platform_host, owner_key, name_key,
			resource_type, resource_number
		) DO UPDATE SET
			etag = excluded.etag,
			fetched_at = excluded.fetched_at`,
		platform, platformHost, owner, name, resourceType, resourceNumber, etag,
	)
	if err != nil {
		return fmt.Errorf("upsert http etag: %w", err)
	}
	return nil
}

// --- Detail Fetch Tracking ---

// UpdateMRDetailFetched marks a merge request as having had its
// detail fetched and records whether CI had pending checks.
func (d *DB) UpdateMRDetailFetched(
	ctx context.Context,
	platform, platformHost, repoOwner, repoName string,
	number int, ciHadPending bool,
) error {
	platform = canonicalRepoPlatform(platform)
	platformHost, repoOwner, repoName = canonicalRepoLookupIdentifier(
		platformHost, repoOwner, repoName,
	)
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET detail_fetched_at = datetime('now'),
		    ci_had_pending = ?
		WHERE repo_id = (
		    SELECT id FROM forge_repos
		    WHERE platform = ? AND platform_host = ? AND owner_key = ? AND name_key = ?
		      AND lifecycle_state = 'active'
		) AND number = ?`,
		ciHadPending, platform, platformHost, repoOwner, repoName, number,
	)
	if err != nil {
		return fmt.Errorf("update mr detail fetched: %w", err)
	}
	return nil
}

// UpdateMRDetailFetchedByRepoID marks a merge request as having had its
// detail fetched for an already resolved provider-qualified repo row.
func (d *DB) UpdateMRDetailFetchedByRepoID(
	ctx context.Context,
	repoID int64,
	number int,
	ciHadPending bool,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET detail_fetched_at = datetime('now'),
		    ci_had_pending = ?
		WHERE repo_id = ? AND number = ?`,
		ciHadPending, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update mr detail fetched by repo id: %w", err)
	}
	return nil
}

// UpdateMRWorkflowApproval persists the workflow-approval snapshot
// for a merge request. The result is tied to headSHA: a later GET
// must compare the stored head SHA to the merge request's current
// PlatformHeadSHA and only trust the snapshot when they match.
// checkedAt is normalized to UTC so SQLite text ordering stays sane.
func (d *DB) UpdateMRWorkflowApproval(
	ctx context.Context,
	repoID int64,
	number int,
	checkedAt time.Time,
	headSHA string,
	required bool,
	count int,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_merge_requests
		SET workflow_approval_checked_at = ?,
		    workflow_approval_head_sha   = ?,
		    workflow_approval_required   = ?,
		    workflow_approval_count      = ?
		WHERE repo_id = ? AND number = ?`,
		checkedAt.UTC(), headSHA, required, count, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("update mr workflow approval: %w", err)
	}
	return nil
}

// UpdateIssueDetailFetched marks an issue as having had its
// detail fetched.
func (d *DB) UpdateIssueDetailFetched(
	ctx context.Context,
	platform, platformHost, repoOwner, repoName string, number int,
) error {
	platform = canonicalRepoPlatform(platform)
	platformHost, repoOwner, repoName = canonicalRepoLookupIdentifier(
		platformHost, repoOwner, repoName,
	)
	_, err := d.execContext(ctx, `
		UPDATE forge_issues
		SET detail_fetched_at = datetime('now')
		WHERE repo_id = (
		    SELECT id FROM forge_repos
		    WHERE platform = ? AND platform_host = ? AND owner_key = ? AND name_key = ?
		      AND lifecycle_state = 'active'
		) AND number = ?`,
		platform, platformHost, repoOwner, repoName, number,
	)
	if err != nil {
		return fmt.Errorf("update issue detail fetched: %w", err)
	}
	return nil
}

// --- Issue Events ---

// UpsertIssueEvents bulk-inserts issue events after normalizing CreatedAt to
// UTC. Duplicate keys refresh mutable fields so edited events and older local
// timestamp encodings are repaired during normal sync.
func (d *DB) UpsertIssueEvents(ctx context.Context, events []IssueEvent) error {
	if len(events) == 0 {
		return nil
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return upsertIssueEventsTx(ctx, tx, events)
	})
}

func upsertIssueEventsTx(ctx context.Context, tx *sql.Tx, events []IssueEvent) error {
	if len(events) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO forge_issue_events
			    (issue_id, platform_id, platform_external_id, event_type, author, summary, body,
			     metadata_json, created_at, dedupe_key, direct_url, thread_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(issue_id, dedupe_key) DO UPDATE SET
			    issue_id       = excluded.issue_id,
			    platform_id    = excluded.platform_id,
			    platform_external_id = excluded.platform_external_id,
			    event_type     = excluded.event_type,
			    author         = excluded.author,
			    summary        = excluded.summary,
			    body           = excluded.body,
			    metadata_json  = excluded.metadata_json,
			    created_at     = excluded.created_at,
			    direct_url     = COALESCE(NULLIF(excluded.direct_url, ''), direct_url),
			    thread_id  = COALESCE(excluded.thread_id, thread_id)`)
	if err != nil {
		return fmt.Errorf("prepare upsert issue events: %w", err)
	}
	defer stmt.Close()
	refStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO forge_issue_pr_references (
			issue_id, source_provider, source_platform_host,
			source_owner, source_repo, source_number, source_url,
			observed_event_key, observed_at
		)
		SELECT
			i.id, r.platform, r.platform_host, ?, ?, ?, ?, ?, ?
		FROM forge_issues i
		JOIN forge_repos r ON r.id = i.repo_id
		WHERE i.id = ?
		ON CONFLICT (
			issue_id, source_provider, source_platform_host,
			source_owner, source_repo, source_number
		) DO UPDATE SET
			source_url = excluded.source_url,
			observed_event_key = excluded.observed_event_key,
			observed_at = MAX(observed_at, excluded.observed_at)`)
	if err != nil {
		return fmt.Errorf("prepare materialize issue PR references: %w", err)
	}
	defer refStmt.Close()

	for i := range events {
		e := &events[i]
		canonicalizeIssueEventTimestamps(e)
		if _, err := stmt.ExecContext(ctx,
			e.IssueID, e.PlatformID, e.PlatformExternalID, e.EventType, e.Author,
			e.Summary, e.Body, e.MetadataJSON, e.CreatedAt,
			e.DedupeKey, e.DirectURL, e.ThreadID,
		); err != nil {
			return fmt.Errorf("insert issue event (dedupe_key=%s): %w", e.DedupeKey, err)
		}
		ref, ok := issuePRReferenceFromEvent(*e)
		if !ok {
			continue
		}
		if _, err := refStmt.ExecContext(
			ctx, ref.SourceOwner, ref.SourceRepo, ref.SourceNumber,
			ref.SourceURL, e.DedupeKey, e.CreatedAt, e.IssueID,
		); err != nil {
			return fmt.Errorf("materialize issue PR reference (dedupe_key=%s): %w", e.DedupeKey, err)
		}
	}
	return nil
}

type issuePRReferenceMetadata struct {
	SourceType   string `json:"source_type"`
	SourceOwner  string `json:"source_owner"`
	SourceRepo   string `json:"source_repo"`
	SourceNumber int    `json:"source_number"`
	SourceURL    string `json:"source_url"`
}

func issuePRReferenceFromEvent(event IssueEvent) (issuePRReferenceMetadata, bool) {
	if event.EventType != "cross_referenced" {
		return issuePRReferenceMetadata{}, false
	}
	var metadata issuePRReferenceMetadata
	if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
		return issuePRReferenceMetadata{}, false
	}
	valid := metadata.SourceType == "PullRequest" &&
		strings.TrimSpace(metadata.SourceOwner) != "" &&
		strings.TrimSpace(metadata.SourceRepo) != "" &&
		metadata.SourceNumber > 0 && strings.TrimSpace(metadata.SourceURL) != ""
	return metadata, valid
}

func (d *DB) IssueCommentEventExists(
	ctx context.Context,
	issueID int64,
	platformID int64,
) (bool, error) {
	var exists bool
	err := d.roQueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM forge_issue_events
			WHERE issue_id = ?
			  AND platform_id = ?
			  AND event_type = 'issue_comment'
		)`,
		issueID,
		platformID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check issue comment event exists: %w", err)
	}
	return exists, nil
}

// ReplaceIssueCommentEvents atomically replaces provider issue-comment events
// and their parent issue's derived comment count.
func (d *DB) ReplaceIssueCommentEvents(
	ctx context.Context,
	issueID int64,
	events []IssueEvent,
	lastActivityAt *time.Time,
) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return replaceIssueCommentEventsTx(ctx, tx, issueID, events, lastActivityAt)
	})
}

func replaceIssueCommentEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	issueID int64,
	events []IssueEvent,
	lastActivityAt *time.Time,
) error {
	query := `DELETE FROM forge_issue_events
			WHERE issue_id = ? AND event_type = 'issue_comment'`
	args := []any{issueID}
	if len(events) > 0 {
		query += ` AND dedupe_key NOT IN (` + sqlPlaceholders(len(events)) + `)`
		for i := range events {
			args = append(args, events[i].DedupeKey)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete missing issue comment events: %w", err)
	}
	if err := upsertIssueEventsTx(ctx, tx, events); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
			UPDATE forge_issues
			SET comment_count = (
				SELECT COUNT(*) FROM forge_issue_events
				WHERE issue_id = ? AND event_type = 'issue_comment'
			), last_activity_at = COALESCE(?, last_activity_at)
			WHERE id = ?`, issueID, lastActivityAt, issueID); err != nil {
		return fmt.Errorf("update issue derived fields: %w", err)
	}
	return nil
}

// ListIssueEvents returns all events for an issue ordered by created_at DESC.
func (d *DB) ListIssueEvents(ctx context.Context, issueID int64) ([]IssueEvent, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, issue_id, platform_id, platform_external_id, event_type, author, summary, body,
		       metadata_json, created_at, dedupe_key, direct_url, thread_id
		FROM forge_issue_events
		WHERE issue_id = ?
		ORDER BY created_at DESC`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("list issue events: %w", err)
	}
	defer rows.Close()

	var events []IssueEvent
	for rows.Next() {
		var e IssueEvent
		var createdAtStr string
		if err := rows.Scan(
			&e.ID, &e.IssueID, &e.PlatformID, &e.PlatformExternalID, &e.EventType, &e.Author,
			&e.Summary, &e.Body, &e.MetadataJSON, &createdAtStr, &e.DedupeKey, &e.DirectURL, &e.ThreadID,
		); err != nil {
			return nil, fmt.Errorf("scan issue event: %w", err)
		}
		t, err := parseDBTime(createdAtStr)
		if err != nil {
			return nil, fmt.Errorf(
				"parse issue event created_at %q: %w",
				createdAtStr, err)
		}
		e.CreatedAt = t
		events = append(events, e)
	}
	return events, rows.Err()
}

// CommentAutocompleteItem identifies the item a comment is being written on.
// Its author, assignees, requested reviewers, and event authors rank ahead of
// other repository users in mention suggestions.
type CommentAutocompleteItem struct {
	Kind   string // "pull" or "issue"
	Number int64
}

// ListCommentAutocompleteUsers returns repo-scoped username suggestions for comment mentions.
// When item is non-nil, users already participating in that item sort first.
func (d *DB) ListCommentAutocompleteUsers(
	ctx context.Context,
	platform, platformHost, owner, name, query string,
	item *CommentAutocompleteItem,
	limit int,
) ([]string, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	if limit <= 0 {
		limit = 10
	}
	query = strings.TrimSpace(query)
	containsQuery := "%" + strings.ToLower(query) + "%"
	prefixQuery := strings.ToLower(query) + "%"
	itemKind := ""
	var itemNumber int64
	if item != nil {
		itemKind = item.Kind
		itemNumber = item.Number
	}

	rows, err := d.roQueryContext(ctx, `
		WITH repo AS (
			SELECT id
			FROM forge_repos
			WHERE platform = ? AND platform_host = ? AND owner_key = ? AND name_key = ?
			  AND lifecycle_state = 'active'
		), current_mr AS (
			SELECT mr.id, mr.author, mr.assignees_json, mr.reviewers_json, mr.last_activity_at
			FROM forge_merge_requests mr
			WHERE ? = 'pull' AND mr.repo_id = (SELECT id FROM repo) AND mr.number = ?
		), current_issue AS (
			SELECT i.id, i.author, i.assignees_json, i.last_activity_at
			FROM forge_issues i
			WHERE ? = 'issue' AND i.repo_id = (SELECT id FROM repo) AND i.number = ?
		), participants AS (
			SELECT author AS login, last_activity_at AS last_seen FROM current_mr
			UNION
			SELECT json_each.value, current_mr.last_activity_at FROM current_mr, json_each(
				CASE WHEN json_valid(current_mr.assignees_json) THEN current_mr.assignees_json ELSE '[]' END
			)
			UNION
			SELECT json_each.value, current_mr.last_activity_at FROM current_mr, json_each(
				CASE WHEN json_valid(current_mr.reviewers_json) THEN current_mr.reviewers_json ELSE '[]' END
			)
			UNION
			SELECT e.author, e.created_at FROM forge_mr_events e
			WHERE e.merge_request_id = (SELECT id FROM current_mr)
			UNION
			SELECT author AS login, last_activity_at AS last_seen FROM current_issue
			UNION
			SELECT json_each.value, current_issue.last_activity_at FROM current_issue, json_each(
				CASE WHEN json_valid(current_issue.assignees_json) THEN current_issue.assignees_json ELSE '[]' END
			)
			UNION
			SELECT e.author, e.created_at FROM forge_issue_events e
			WHERE e.issue_id = (SELECT id FROM current_issue)
		), candidates AS (
			SELECT login, last_seen FROM participants
			UNION ALL
			SELECT mr.author AS login, mr.last_activity_at AS last_seen
			FROM forge_merge_requests mr
			WHERE mr.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
				SELECT 1 FROM forge_archive_items a
				WHERE a.repo_id = mr.repo_id
				  AND a.item_type = 'merge_request'
				  AND a.item_number = mr.number
				  AND a.lifecycle_state = 'removed_upstream'
			  )
			UNION ALL
			SELECT i.author AS login, i.last_activity_at AS last_seen
			FROM forge_issues i
			WHERE i.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
				SELECT 1 FROM forge_archive_items a
				WHERE a.repo_id = i.repo_id
				  AND a.item_type = 'issue'
				  AND a.item_number = i.number
				  AND a.lifecycle_state = 'removed_upstream'
			  )
			UNION ALL
			SELECT e.author AS login, e.created_at AS last_seen
			FROM forge_mr_events e
			JOIN forge_merge_requests mr ON mr.id = e.merge_request_id
			WHERE mr.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
				SELECT 1 FROM forge_archive_items a
				WHERE a.repo_id = mr.repo_id
				  AND a.item_type = 'merge_request'
				  AND a.item_number = mr.number
				  AND a.lifecycle_state = 'removed_upstream'
			  )
			UNION ALL
			SELECT e.author AS login, e.created_at AS last_seen
			FROM forge_issue_events e
			JOIN forge_issues i ON i.id = e.issue_id
			WHERE i.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
				SELECT 1 FROM forge_archive_items a
				WHERE a.repo_id = i.repo_id
				  AND a.item_type = 'issue'
				  AND a.item_number = i.number
				  AND a.lifecycle_state = 'removed_upstream'
			  )
		), ranked AS (
			SELECT login, MAX(last_seen) AS last_seen,
			  EXISTS (SELECT 1 FROM participants p WHERE p.login = candidates.login) AS participant
			FROM candidates
			WHERE login <> ''
			  AND (? = '' OR LOWER(login) LIKE ?)
			GROUP BY login
		)
		SELECT login
		FROM ranked
		ORDER BY
			participant DESC,
			CASE WHEN ? <> '' AND LOWER(login) LIKE ? THEN 0 ELSE 1 END,
			last_seen DESC,
			login ASC
		LIMIT ?`,
		platform, platformHost, owner, name,
		itemKind, itemNumber,
		itemKind, itemNumber,
		query, containsQuery,
		query, prefixQuery,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list comment autocomplete users: %w", err)
	}
	defer rows.Close()

	users := make([]string, 0, limit)
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, fmt.Errorf("scan comment autocomplete user: %w", err)
		}
		users = append(users, login)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate comment autocomplete users: %w", err)
	}
	return users, nil
}

// ListCommentAutocompleteReferences returns repo-scoped item reference suggestions.
func (d *DB) ListCommentAutocompleteReferences(
	ctx context.Context,
	platform, platformHost, owner, name, query, itemKind string,
	limit int,
) ([]CommentAutocompleteReference, error) {
	platform = canonicalRepoPlatform(platform)
	platformHost, owner, name = canonicalRepoLookupIdentifier(platformHost, owner, name)
	if limit <= 0 {
		limit = 10
	}
	query = strings.TrimSpace(query)
	itemKind = strings.TrimSpace(itemKind)
	titleQuery := "%" + strings.ToLower(query) + "%"
	numberPrefix := query + "%"

	rows, err := d.roQueryContext(ctx, `
		WITH repo AS (
			SELECT id
			FROM forge_repos
			WHERE platform = ? AND platform_host = ? AND owner_key = ? AND name_key = ?
			  AND lifecycle_state = 'active'
		), candidates AS (
			SELECT 'pull' AS kind, mr.number, mr.title, mr.state, mr.last_activity_at
			FROM forge_merge_requests mr
			WHERE mr.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
			      SELECT 1 FROM forge_archive_items ai
			      WHERE ai.repo_id = mr.repo_id
			        AND ai.item_type = 'merge_request'
			        AND ai.item_number = mr.number
			        AND ai.lifecycle_state = 'removed_upstream'
			  )
			UNION ALL
			SELECT 'issue' AS kind, i.number, i.title, i.state, i.last_activity_at
			FROM forge_issues i
			WHERE i.repo_id = (SELECT id FROM repo)
			  AND NOT EXISTS (
			      SELECT 1 FROM forge_archive_items ai
			      WHERE ai.repo_id = i.repo_id
			        AND ai.item_type = 'issue'
			        AND ai.item_number = i.number
			        AND ai.lifecycle_state = 'removed_upstream'
			  )
		)
		SELECT kind, number, title, state
		FROM candidates
		WHERE (? = '' OR kind = ?)
		  AND (
		       ? = ''
		    OR CAST(number AS TEXT) LIKE ?
		    OR LOWER(title) LIKE ?
		  )
		ORDER BY
			CASE WHEN ? <> '' AND CAST(number AS TEXT) LIKE ? THEN 0 ELSE 1 END,
			CASE WHEN ? <> '' AND LOWER(title) LIKE ? THEN 0 ELSE 1 END,
			last_activity_at DESC,
			number DESC
		LIMIT ?`,
		platform, platformHost, owner, name,
		itemKind, itemKind,
		query, numberPrefix, titleQuery,
		query, numberPrefix,
		query, titleQuery,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list comment autocomplete references: %w", err)
	}
	defer rows.Close()

	references := make([]CommentAutocompleteReference, 0, limit)
	for rows.Next() {
		var ref CommentAutocompleteReference
		if err := rows.Scan(&ref.Kind, &ref.Number, &ref.Title, &ref.State); err != nil {
			return nil, fmt.Errorf("scan comment autocomplete reference: %w", err)
		}
		references = append(references, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate comment autocomplete references: %w", err)
	}
	return references, nil
}

// --- Starring ---

// SetStarred stars an item (MR or issue).
func (d *DB) SetStarred(
	ctx context.Context, itemType string, repoID int64, number int,
) error {
	_, err := d.execContext(ctx, `
		INSERT INTO forge_starred_items (item_type, repo_id, number)
		VALUES (?, ?, ?)
		ON CONFLICT(item_type, repo_id, number) DO NOTHING`,
		itemType, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("set starred: %w", err)
	}
	return nil
}

// UnsetStarred removes a star from an item.
func (d *DB) UnsetStarred(
	ctx context.Context, itemType string, repoID int64, number int,
) error {
	_, err := d.execContext(ctx, `
		DELETE FROM forge_starred_items
		WHERE item_type = ? AND repo_id = ? AND number = ?`,
		itemType, repoID, number,
	)
	if err != nil {
		return fmt.Errorf("unset starred: %w", err)
	}
	return nil
}

// IsStarred checks whether an item is starred.
func (d *DB) IsStarred(
	ctx context.Context, itemType string, repoID int64, number int,
) (bool, error) {
	var count int
	err := d.roQueryRowContext(ctx, `
		SELECT COUNT(*) FROM forge_starred_items
		WHERE item_type = ? AND repo_id = ? AND number = ?`,
		itemType, repoID, number,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("is starred: %w", err)
	}
	return count > 0, nil
}

// --- Rate Limits ---

// UpsertRateLimit inserts or updates a GitHub identity rate limit row.
func (d *DB) UpsertRateLimit(
	platformHost string,
	ratePrincipal string,
	apiType string,
	requestsHour int,
	hourStart time.Time,
	rateRemaining int,
	rateLimit int,
	rateResetAt *time.Time,
) error {
	return d.UpsertPlatformRateLimit(
		"github", platformHost, ratePrincipal, apiType, requestsHour, hourStart,
		rateRemaining, rateLimit, rateResetAt,
	)
}

// UpsertPlatformRateLimit inserts or updates a principal-scoped rate row.
func (d *DB) UpsertPlatformRateLimit(
	platform string,
	platformHost string,
	ratePrincipal string,
	apiType string,
	requestsHour int,
	hourStart time.Time,
	rateRemaining int,
	rateLimit int,
	rateResetAt *time.Time,
) error {
	_, err := d.rwExecContext(context.Background(), `
		INSERT INTO forge_rate_limits
		    (platform, platform_host, rate_principal, api_type, requests_hour,
		     hour_start, rate_remaining, rate_limit, rate_reset_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(platform, platform_host, rate_principal, api_type) DO UPDATE SET
		    requests_hour  = excluded.requests_hour,
		    hour_start     = excluded.hour_start,
		    rate_remaining = excluded.rate_remaining,
		    rate_limit     = excluded.rate_limit,
		    rate_reset_at  = excluded.rate_reset_at,
		    updated_at     = datetime('now')`,
		platform, platformHost, ratePrincipal, apiType, requestsHour, hourStart,
		rateRemaining, rateLimit, rateResetAt,
	)
	if err != nil {
		return fmt.Errorf("upsert rate limit: %w", err)
	}
	return nil
}

// GetRateLimit returns one GitHub identity rate-limit row.
func (d *DB) GetRateLimit(
	platformHost string,
	ratePrincipal string,
	apiType string,
) (*RateLimit, error) {
	return d.GetPlatformRateLimit("github", platformHost, ratePrincipal, apiType)
}

// GetPlatformRateLimit returns the row for a provider, host, principal, and API.
func (d *DB) GetPlatformRateLimit(
	platform string,
	platformHost string,
	ratePrincipal string,
	apiType string,
) (*RateLimit, error) {
	var r RateLimit
	err := d.roQueryRowContext(context.Background(), `
		SELECT id, platform, platform_host, rate_principal, api_type,
		       requests_hour, hour_start, rate_remaining, rate_limit,
		       rate_reset_at, updated_at
		FROM forge_rate_limits
		WHERE platform = ? AND platform_host = ? AND rate_principal = ? AND api_type = ?`,
		platform, platformHost, ratePrincipal, apiType,
	).Scan(
		&r.ID, &r.Platform, &r.PlatformHost, &r.RatePrincipal, &r.APIType,
		&r.RequestsHour, &r.HourStart, &r.RateRemaining, &r.RateLimit,
		&r.RateResetAt, &r.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get rate limit: %w", err)
	}
	return &r, nil
}

// --- Worktree Links ---

// SetWorktreeLinks replaces all worktree links atomically.
func (d *DB) SetWorktreeLinks(
	ctx context.Context, links []WorktreeLink,
) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM forge_mr_worktree_links`,
		); err != nil {
			return fmt.Errorf("delete worktree links: %w", err)
		}
		if len(links) == 0 {
			return nil
		}
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO forge_mr_worktree_links
			    (merge_request_id, worktree_key,
			     worktree_path, worktree_branch, linked_at)
			VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf(
				"prepare insert worktree link: %w", err,
			)
		}
		defer stmt.Close()
		for i := range links {
			l := &links[i]
			if _, err := stmt.ExecContext(ctx,
				l.MergeRequestID, l.WorktreeKey,
				l.WorktreePath, l.WorktreeBranch,
				l.LinkedAt.UTC().Format(time.RFC3339),
			); err != nil {
				return fmt.Errorf(
					"insert worktree link %s: %w",
					l.WorktreeKey, err,
				)
			}
		}
		return nil
	})
}

// GetWorktreeLinksForMR returns worktree links for a
// specific merge request.
func (d *DB) GetWorktreeLinksForMR(
	ctx context.Context, mergeRequestID int64,
) ([]WorktreeLink, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, merge_request_id, worktree_key,
		       worktree_path, worktree_branch, linked_at
		FROM forge_mr_worktree_links
		WHERE merge_request_id = ?
		ORDER BY linked_at DESC`,
		mergeRequestID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"get worktree links for MR: %w", err,
		)
	}
	defer rows.Close()
	return scanWorktreeLinks(rows)
}

// GetWorktreeLinksForMRs returns worktree links for the
// given merge request IDs. IDs are batched to stay within
// SQLite's bind-parameter limit.
func (d *DB) GetWorktreeLinksForMRs(
	ctx context.Context, mrIDs []int64,
) ([]WorktreeLink, error) {
	if len(mrIDs) == 0 {
		return nil, nil
	}
	const batchSize = 500
	var all []WorktreeLink
	for start := 0; start < len(mrIDs); start += batchSize {
		end := min(start+batchSize, len(mrIDs))
		batch := mrIDs[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `
			SELECT id, merge_request_id, worktree_key,
			       worktree_path, worktree_branch, linked_at
			FROM forge_mr_worktree_links
			WHERE merge_request_id IN (` +
			strings.Join(placeholders, ",") + `)
			ORDER BY linked_at DESC`
		rows, err := d.roQueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf(
				"get worktree links for MRs: %w", err,
			)
		}
		links, err := scanWorktreeLinks(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, links...)
	}
	return all, nil
}

// GetAllWorktreeLinks returns all worktree links ordered
// by linked_at DESC.
func (d *DB) GetAllWorktreeLinks(
	ctx context.Context,
) ([]WorktreeLink, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, merge_request_id, worktree_key,
		       worktree_path, worktree_branch, linked_at
		FROM forge_mr_worktree_links
		ORDER BY linked_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"get all worktree links: %w", err,
		)
	}
	defer rows.Close()
	return scanWorktreeLinks(rows)
}

// --- Workspaces ---

func canonicalWorkspacePlatform(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "github"
	}
	return provider
}

func (d *DB) workspaceRouteHasHistoricalOccupants(
	ctx context.Context,
	provider, platformHost, repoPathKey string,
) (bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var collision bool
	// A route is ambiguous when any of its records belongs to a repository
	// other than its current occupant — including a vacated route whose
	// only records are historical (rename observed, replacement not yet
	// cataloged). Legacy route-only repositories (no provider ID) record
	// their route as non-current without being vacated, so a record with
	// no occupant counts only when its repository is cataloged.
	err := d.roQueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM forge_repo_routes historical
			WHERE historical.platform_host = ?
			  AND historical.repo_path_key = ?
			  AND (? = '' OR historical.platform = ?)
			  AND NOT EXISTS (
				SELECT 1
				FROM forge_repo_routes current
				WHERE current.platform = historical.platform
				  AND current.platform_host = historical.platform_host
				  AND current.repo_path_key = historical.repo_path_key
				  AND current.is_current = 1
				  AND current.repo_id = historical.repo_id
			  )
			  AND (
				EXISTS (
					SELECT 1
					FROM forge_repo_routes occupant
					WHERE occupant.platform = historical.platform
					  AND occupant.platform_host = historical.platform_host
					  AND occupant.repo_path_key = historical.repo_path_key
					  AND occupant.is_current = 1
				)
				OR EXISTS (
					SELECT 1
					FROM forge_repos repo
					WHERE repo.id = historical.repo_id
					  AND repo.platform_repo_id <> ''
				)
			  )
		)`,
		platformHost, repoPathKey, provider, provider,
	).Scan(&collision)
	if err != nil {
		return false, fmt.Errorf("inspect workspace repository route: %w", err)
	}
	return collision, nil
}

// WorkspaceRepoRouteHasHistoricalOccupants reports whether the given
// repository route has ever belonged to a repository other than its current
// occupant. Operational workspace paths use it to fail closed instead of
// fetching code from a route's new occupant.
func (d *DB) WorkspaceRepoRouteHasHistoricalOccupants(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (bool, error) {
	host, ownerKey, nameKey := canonicalRepoLookupIdentifier(
		platformHost, owner, name,
	)
	return d.workspaceRouteHasHistoricalOccupants(
		ctx, provider, host, ownerKey+"/"+nameKey,
	)
}

func (d *DB) canonicalizeWorkspaceRepo(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (string, string, string, string, string, string, string, int64, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	host, ownerKey, nameKey := canonicalRepoLookupIdentifier(platformHost, owner, name)
	pathKey := ownerKey + "/" + nameKey

	var matchedProvider, displayOwner, displayName, repoOwnerKey, repoNameKey, repoPathKey string
	var repoID int64
	err := d.roQueryRowContext(ctx, `
		SELECT platform, owner, name, owner_key, name_key, repo_path_key, id
		FROM forge_repos
		WHERE platform_host = ? AND repo_path_key = ?
		  AND lifecycle_state = 'active'
		  AND (? = '' OR platform = ?)
		ORDER BY CASE WHEN platform <> 'github' THEN 0 ELSE 1 END, id
		LIMIT 1`,
		host, pathKey, provider, provider,
	).Scan(
		&matchedProvider, &displayOwner, &displayName,
		&repoOwnerKey, &repoNameKey, &repoPathKey, &repoID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return canonicalWorkspacePlatform(provider), host, ownerKey, nameKey,
			ownerKey, nameKey, pathKey, 0, nil
	}
	if err != nil {
		return "", "", "", "", "", "", "", 0,
			fmt.Errorf("lookup workspace repo identity: %w", err)
	}
	return matchedProvider, host, displayOwner, displayName,
		repoOwnerKey, repoNameKey, repoPathKey, repoID, nil
}

func (d *DB) resolveWorkspaceLookupRoute(
	ctx context.Context,
	provider, platformHost, owner, name string,
) (string, string, string, int64, bool, error) {
	_, host, _, _, ownerKey, nameKey, pathKey, repoID, err :=
		d.canonicalizeWorkspaceRepo(ctx, provider, platformHost, owner, name)
	if err != nil {
		return "", "", "", 0, false, err
	}
	if repoID != 0 {
		return host, ownerKey, nameKey, repoID, false, nil
	}
	collision, err := d.workspaceRouteHasHistoricalOccupants(
		ctx, provider, host, pathKey,
	)
	if err != nil {
		return "", "", "", 0, false, err
	}
	return host, ownerKey, nameKey, 0, !collision, nil
}

func workspaceRepositoryLookupPredicate(
	provider, platformHost, owner, name string, repoID int64,
) (string, []any) {
	if repoID != 0 {
		return "repo_id = ?", []any{repoID}
	}
	predicate := `repo_id IS NULL
		AND platform_host = ? AND repo_owner_key = ? AND repo_name_key = ?`
	args := []any{platformHost, owner, name}
	if provider != "" {
		predicate += " AND platform = ?"
		args = append(args, provider)
	}
	return predicate, args
}

func workspaceItemKeyForInsert(ws *Workspace) (string, error) {
	itemKey := strings.TrimSpace(ws.ItemKey)
	if itemKey != "" {
		return itemKey, nil
	}
	if ws.ItemType == WorkspaceItemTypeKataTask {
		return "", errors.New("kata task workspace item_key is required")
	}
	// Ad-hoc workspaces have no item number, so the number fallback below
	// would key every one of them in a repository as "0" and collide on the
	// (platform, host, repo, item_type, item_key) unique index.
	if ws.ItemType == WorkspaceItemTypeAdHoc {
		return "", errors.New("ad-hoc workspace item_key is required")
	}
	return strconv.Itoa(ws.ItemNumber), nil
}

// workspaceItemTypeKeysByNumber reports whether an item type's workspace key
// is derived from its item number. Kata-task and ad-hoc workspaces have no
// meaningful item number and always carry an explicit item_key.
func workspaceItemTypeKeysByNumber(itemType string) bool {
	return itemType != WorkspaceItemTypeKataTask &&
		itemType != WorkspaceItemTypeAdHoc
}

func workspaceKataMetadataJSON(ws *Workspace) (string, error) {
	if ws.KataMetadata == nil {
		return "", nil
	}
	data, err := json.Marshal(ws.KataMetadata)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (d *DB) scanWorkspace(
	ctx context.Context,
	scanner interface{ Scan(...any) error },
) (*Workspace, error) {
	ws, err := scanWorkspaceRow(scanner)
	if err != nil {
		return nil, err
	}
	return d.resolveWorkspaceRepository(ctx, ws)
}

func (d *DB) resolveWorkspaceRepository(
	ctx context.Context, ws *Workspace,
) (*Workspace, error) {
	if ws.RepoID == 0 {
		return ws, nil
	}
	repo, err := d.GetActiveRepoByID(ctx, ws.RepoID)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace repository route: %w", err)
	}
	if repo != nil {
		ws.Platform = repo.Platform
		ws.PlatformHost = repo.PlatformHost
		ws.RepoOwner = repo.Owner
		ws.RepoName = repo.Name
	}
	return ws, nil
}

func scanWorkspaceRow(
	scanner interface{ Scan(...any) error },
) (*Workspace, error) {
	var ws Workspace
	var kataMetadataJSON string
	var repoID sql.NullInt64
	err := scanner.Scan(
		&ws.ID, &ws.Platform, &ws.PlatformHost, &ws.RepoOwner, &ws.RepoName,
		&repoID,
		&ws.ItemType, &ws.ItemNumber, &ws.ItemKey, &ws.AssociatedPRNumber,
		&ws.GitHeadRef, &ws.MRHeadRepo, &ws.WorkspaceBranch,
		&ws.WorktreePath, &ws.TmuxSession, &ws.TerminalBackend, &ws.Status,
		&ws.ErrorMessage, &ws.CreatedAt, &kataMetadataJSON,
	)
	if err != nil {
		return nil, err
	}
	if repoID.Valid {
		ws.RepoID = repoID.Int64
	}
	if ws.ItemKey == "" && workspaceItemTypeKeysByNumber(ws.ItemType) {
		ws.ItemKey = strconv.Itoa(ws.ItemNumber)
	}
	ws.CreatedAt = ws.CreatedAt.UTC()
	if strings.TrimSpace(kataMetadataJSON) != "" {
		var metadata WorkspaceKataMetadata
		if err := json.Unmarshal([]byte(kataMetadataJSON), &metadata); err != nil {
			return nil, fmt.Errorf("decode workspace kata metadata: %w", err)
		}
		ws.KataMetadata = &metadata
	}
	return &ws, nil
}

// InsertWorkspace inserts a new workspace row.
func (d *DB) InsertWorkspace(
	ctx context.Context, ws *Workspace,
) error {
	// The route-history collision check and the insert must be atomic with
	// respect to repository reconciliation, or a concurrent replacement can
	// slip a workspace onto a route that just became historically ambiguous.
	release, err := d.LockRepositoryReconciliationRead(ctx)
	if err != nil {
		return err
	}
	defer release()
	prepared, err := d.prepareWorkspaceInsert(ctx, ws)
	if err != nil {
		return err
	}
	if err := insertPreparedWorkspace(ctx, d.rwStmts, ws, prepared); err != nil {
		return err
	}
	ws.ItemKey = prepared.itemKey
	return nil
}

type preparedWorkspaceInsert struct {
	repoOwnerKey     string
	repoNameKey      string
	repoPathKey      string
	repoID           int64
	itemKey          string
	kataMetadataJSON string
}

func (d *DB) prepareWorkspaceInsert(
	ctx context.Context, ws *Workspace,
) (preparedWorkspaceInsert, error) {
	if ws == nil {
		return preparedWorkspaceInsert{}, errors.New("insert workspace: workspace is required")
	}
	var prepared preparedWorkspaceInsert
	var err error
	requestedRepoID := ws.RepoID
	ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		prepared.repoOwnerKey, prepared.repoNameKey, prepared.repoPathKey,
		prepared.repoID, err =
		d.canonicalizeWorkspaceRepo(
			ctx, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		)
	if err != nil {
		return preparedWorkspaceInsert{}, err
	}
	if requestedRepoID != 0 && requestedRepoID != prepared.repoID {
		return preparedWorkspaceInsert{}, fmt.Errorf(
			"%w: workspace repository identity changed for route: %s/%s",
			ErrRepositoryRouteFenceChanged,
			ws.RepoOwner, ws.RepoName,
		)
	}
	if prepared.repoID == 0 {
		collision, err := d.workspaceRouteHasHistoricalOccupants(
			ctx, ws.Platform, ws.PlatformHost, prepared.repoPathKey,
		)
		if err != nil {
			return preparedWorkspaceInsert{}, err
		}
		if collision {
			return preparedWorkspaceInsert{}, fmt.Errorf(
				"workspace repository route has historical occupants: %s/%s",
				prepared.repoOwnerKey, prepared.repoNameKey,
			)
		}
	}
	ws.RepoID = prepared.repoID
	if ws.TerminalBackend == "" {
		ws.TerminalBackend = "tmux"
	}
	prepared.itemKey, err = workspaceItemKeyForInsert(ws)
	if err != nil {
		return preparedWorkspaceInsert{}, fmt.Errorf("insert workspace: %w", err)
	}
	prepared.kataMetadataJSON, err = workspaceKataMetadataJSON(ws)
	if err != nil {
		return preparedWorkspaceInsert{}, fmt.Errorf("encode workspace kata metadata: %w", err)
	}
	return prepared, nil
}

type workspaceInsertExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertPreparedWorkspace(
	ctx context.Context,
	executor workspaceInsertExecutor,
	ws *Workspace,
	prepared preparedWorkspaceInsert,
) error {
	_, err := executor.ExecContext(ctx, `
		INSERT INTO forge_workspaces
		    (id, platform, platform_host, repo_owner, repo_name, repo_id,
		     repo_owner_key, repo_name_key, repo_path_key,
		     item_type, item_number, item_key, associated_pr_number,
		     git_head_ref, mr_head_repo, workspace_branch,
		     worktree_path, tmux_session, terminal_backend, status,
		     error_message, kata_metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ws.ID, ws.Platform, ws.PlatformHost, ws.RepoOwner, ws.RepoName,
		sql.NullInt64{Int64: prepared.repoID, Valid: prepared.repoID != 0},
		prepared.repoOwnerKey, prepared.repoNameKey, prepared.repoPathKey,
		ws.ItemType, ws.ItemNumber, prepared.itemKey, ws.AssociatedPRNumber,
		ws.GitHeadRef, ws.MRHeadRepo, ws.WorkspaceBranch,
		ws.WorktreePath, ws.TmuxSession, ws.TerminalBackend, ws.Status,
		ws.ErrorMessage, prepared.kataMetadataJSON,
	)
	if err != nil {
		return fmt.Errorf("insert workspace: %w", err)
	}
	return nil
}

// GetWorkspace returns a workspace by ID, or nil if not found.
func (d *DB) GetWorkspace(
	ctx context.Context, id string,
) (*Workspace, error) {
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
		FROM forge_workspaces WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	return ws, nil
}

// GetWorkspaceByMR returns the workspace for a specific MR,
// or nil if not found.
func (d *DB) GetWorkspaceByMR(
	ctx context.Context,
	platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	return d.getWorkspaceByMR(ctx, "", platformHost, owner, name, mrNumber)
}

// GetWorkspaceByMRForProvider returns the workspace for a specific MR within a
// provider identity, or nil if not found.
func (d *DB) GetWorkspaceByMRForProvider(
	ctx context.Context,
	provider, platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	return d.getWorkspaceByMR(
		ctx, provider, platformHost, owner, name, mrNumber,
	)
}

// GetWorkspaceLinkedToMRForProvider returns the workspace represented on MR
// detail surfaces. A workspace created directly for the MR takes precedence;
// otherwise the newest issue, Kata-task, or ad-hoc workspace with a persisted
// association is returned. Status does not affect selection, and ID ordering
// only makes equal creation timestamps deterministic.
func (d *DB) GetWorkspaceLinkedToMRForProvider(
	ctx context.Context,
	provider, platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	platformHost, owner, name, repoID, legacySafe, err :=
		d.resolveWorkspaceLookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	if repoID == 0 && !legacySafe {
		return nil, nil
	}
	predicate, args := workspaceRepositoryLookupPredicate(
		provider, platformHost, owner, name, repoID,
	)
	query := `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
			FROM forge_workspaces
			WHERE (` + predicate + `)
			  AND (
			    (item_type = ? AND item_number = ?)
			    OR (
			      item_type IN (?, ?, ?)
			      AND associated_pr_number = ?
			    )
			  )
			ORDER BY CASE
			           WHEN item_type = ? AND item_number = ? THEN 0
			           ELSE 1
			         END,
			         created_at DESC,
			         id DESC
			LIMIT 1`
	args = append(args,
		WorkspaceItemTypePullRequest, mrNumber,
		WorkspaceItemTypeIssue, WorkspaceItemTypeKataTask, WorkspaceItemTypeAdHoc,
		mrNumber,
		WorkspaceItemTypePullRequest, mrNumber,
	)
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace linked to MR: %w", err)
	}
	return ws, nil
}

func (d *DB) getWorkspaceByMR(
	ctx context.Context,
	provider, platformHost, owner, name string,
	mrNumber int,
) (*Workspace, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	platformHost, owner, name, repoID, legacySafe, err :=
		d.resolveWorkspaceLookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	if repoID == 0 && !legacySafe {
		return nil, nil
	}
	predicate, lookupArgs := workspaceRepositoryLookupPredicate(
		provider, platformHost, owner, name, repoID,
	)
	query := `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
			FROM forge_workspaces
			WHERE item_type = ? AND item_number = ?
			  AND (` + predicate + `)`
	args := append([]any{WorkspaceItemTypePullRequest, mrNumber}, lookupArgs...)
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace by MR: %w", err)
	}
	return ws, nil
}

// GetWorkspaceByIssue returns the workspace for a specific issue,
// or nil if not found.
func (d *DB) GetWorkspaceByIssue(
	ctx context.Context,
	platformHost, owner, name string,
	issueNumber int,
) (*Workspace, error) {
	return d.getWorkspaceByIssue(ctx, "", platformHost, owner, name, issueNumber)
}

// GetWorkspaceByIssueForProvider returns the workspace for a specific issue
// within a provider identity, or nil if not found.
func (d *DB) GetWorkspaceByIssueForProvider(
	ctx context.Context,
	provider, platformHost, owner, name string,
	issueNumber int,
) (*Workspace, error) {
	return d.getWorkspaceByIssue(
		ctx, provider, platformHost, owner, name, issueNumber,
	)
}

func (d *DB) getWorkspaceByIssue(
	ctx context.Context,
	provider, platformHost, owner, name string,
	issueNumber int,
) (*Workspace, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	platformHost, owner, name, repoID, legacySafe, err :=
		d.resolveWorkspaceLookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	if repoID == 0 && !legacySafe {
		return nil, nil
	}
	predicate, lookupArgs := workspaceRepositoryLookupPredicate(
		provider, platformHost, owner, name, repoID,
	)
	query := `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
		FROM forge_workspaces
		WHERE item_type = ? AND item_number = ?
		  AND (` + predicate + `)`
	args := append([]any{WorkspaceItemTypeIssue, issueNumber}, lookupArgs...)
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace by issue: %w", err)
	}
	return ws, nil
}

func (d *DB) GetWorkspaceByItemKeyForProvider(
	ctx context.Context,
	provider, platformHost, owner, name, itemType, itemKey string,
) (*Workspace, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	itemType = strings.TrimSpace(itemType)
	itemKey = strings.TrimSpace(itemKey)
	if itemType == "" || itemKey == "" {
		return nil, nil
	}
	platformHost, owner, name, repoID, legacySafe, err :=
		d.resolveWorkspaceLookupRoute(ctx, provider, platformHost, owner, name)
	if err != nil {
		return nil, err
	}
	if repoID == 0 && !legacySafe {
		return nil, nil
	}
	predicate, lookupArgs := workspaceRepositoryLookupPredicate(
		provider, platformHost, owner, name, repoID,
	)
	query := `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
		FROM forge_workspaces
		WHERE item_type = ? AND item_key = ?
		  AND (` + predicate + `)`
	args := append([]any{itemType, itemKey}, lookupArgs...)
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace by item key: %w", err)
	}
	return ws, nil
}

// GetKataWorkspaceByIssue returns the existing workspace for a Kata issue's
// daemon-owned identity. Project UID is deliberately excluded because Kata
// issues can move between projects without changing their issue UID.
func (d *DB) GetKataWorkspaceByIssue(
	ctx context.Context,
	daemonID, issueUID string,
) (*Workspace, error) {
	daemonID = strings.TrimSpace(daemonID)
	issueUID = strings.TrimSpace(issueUID)
	if daemonID == "" || issueUID == "" {
		return nil, nil
	}
	ws, err := d.scanWorkspace(ctx, d.roQueryRowContext(ctx, `
		SELECT id, platform, platform_host, repo_owner, repo_name, repo_id,
		       item_type, item_number, item_key, associated_pr_number,
		       git_head_ref, mr_head_repo, workspace_branch,
		       worktree_path, tmux_session, terminal_backend, status,
		       error_message, created_at, kata_metadata
		FROM forge_workspaces
		WHERE item_type = ?
		  AND json_extract(kata_metadata, '$.daemon_id') = ?
		  AND json_extract(kata_metadata, '$.issue_uid') = ?
		ORDER BY created_at ASC, id ASC
		LIMIT 1`, WorkspaceItemTypeKataTask, daemonID, issueUID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Kata workspace by issue: %w", err)
	}
	return ws, nil
}

// ListWorkspaces returns all workspaces ordered by
// created_at DESC.
func (d *DB) ListWorkspaces(
	ctx context.Context,
) ([]Workspace, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT w.id,
		       COALESCE(r.platform, w.platform),
		       COALESCE(r.platform_host, w.platform_host),
		       COALESCE(r.owner, w.repo_owner),
		       COALESCE(r.name, w.repo_name),
		       w.repo_id,
		       w.item_type, w.item_number, w.item_key, w.associated_pr_number,
		       w.git_head_ref, w.mr_head_repo, w.workspace_branch,
		       w.worktree_path, w.tmux_session, w.terminal_backend, w.status,
		       w.error_message, w.created_at, w.kata_metadata
		FROM forge_workspaces w
		LEFT JOIN forge_repos r
		  ON r.id = w.repo_id AND r.lifecycle_state = 'active'
		ORDER BY w.created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		ws, err := scanWorkspaceRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		out = append(out, *ws)
	}
	return out, rows.Err()
}

// UpdateWorkspaceStatus sets the status and optional error
// message for a workspace.
func (d *DB) UpdateWorkspaceStatus(
	ctx context.Context,
	id, status string,
	errMsg *string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = ?, error_message = ?
		WHERE id = ?`,
		status, errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("update workspace status: %w", err)
	}
	return nil
}

// MarkReadyWorkspaceError records a runtime failure only while the workspace
// is still ready. A false result means another lifecycle transition won the
// race and must not be overwritten by the stale failure observation.
func (d *DB) MarkReadyWorkspaceError(
	ctx context.Context, id, message string,
) (bool, error) {
	result, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = 'error', error_message = ?
		WHERE id = ? AND status = 'ready'`,
		message, id,
	)
	if err != nil {
		return false, fmt.Errorf("mark ready workspace error: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read ready workspace error result: %w", err)
	}
	return rowsAffected == 1, nil
}

// BeginWorkspaceDeletion atomically admits one deletion attempt from a stable
// workspace state and clears any error left by an earlier attempt. A false
// result means another deletion is already responsible for the workspace.
func (d *DB) BeginWorkspaceDeletion(ctx context.Context, id string) (bool, error) {
	return d.beginWorkspaceDeletion(ctx, id, true)
}

// BeginWorkspaceRetirement atomically admits an automatic deletion attempt,
// but leaves a prior deletion failure for explicit user action.
func (d *DB) BeginWorkspaceRetirement(ctx context.Context, id string) (bool, error) {
	return d.beginWorkspaceDeletion(ctx, id, false)
}

func (d *DB) beginWorkspaceDeletion(
	ctx context.Context, id string, retryFailure bool,
) (bool, error) {
	query := `
		UPDATE forge_workspaces
		SET status = 'deleting', error_message = NULL
		WHERE id = ? AND status IN ('ready', 'error')`
	if retryFailure {
		query = `
			UPDATE forge_workspaces
			SET status = 'deleting', error_message = NULL
			WHERE id = ? AND status IN ('ready', 'error', 'deletion_failed')`
	}
	result, err := d.execContext(ctx, query, id)
	if err != nil {
		return false, fmt.Errorf("begin workspace deletion: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read workspace deletion result: %w", err)
	}
	if rowsAffected == 1 {
		return true, nil
	}
	var status string
	err = d.roQueryRowContext(ctx, `SELECT status FROM forge_workspaces WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || status == "deleting" ||
		(!retryFailure && status == "deletion_failed") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read workspace status after deletion admission: %w", err)
	}
	if status == "creating" {
		return false, ErrWorkspaceSetupInProgress
	}
	return false, fmt.Errorf("workspace status %q does not permit deletion", status)
}

// FailWorkspaceDeletion preserves a failed teardown as a recoverable row.
func (d *DB) FailWorkspaceDeletion(ctx context.Context, id, message string) error {
	result, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = 'deletion_failed', error_message = ?
		WHERE id = ? AND status = 'deleting'`,
		message, id,
	)
	if err != nil {
		return fmt.Errorf("fail workspace deletion: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read workspace deletion failure result: %w", err)
	}
	if rowsAffected != 1 {
		return fmt.Errorf("fail workspace deletion: workspace is not deleting")
	}
	return nil
}

// FailInterruptedWorkspaceDeletions makes daemon-interrupted destructive work
// explicit. Retrying it always requires another user action.
func (d *DB) FailInterruptedWorkspaceDeletions(ctx context.Context, message string) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = 'deletion_failed', error_message = ?
		WHERE status = 'deleting'`,
		message,
	)
	if err != nil {
		return fmt.Errorf("fail interrupted workspace deletions: %w", err)
	}
	return nil
}

// FailInterruptedWorkspaceSetups makes process-local setup work that could not
// survive a daemon restart explicit and retryable.
func (d *DB) FailInterruptedWorkspaceSetups(ctx context.Context, message string) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = 'error', error_message = ?
		WHERE status = 'creating'`,
		message,
	)
	if err != nil {
		return fmt.Errorf("fail interrupted workspace setups: %w", err)
	}
	return nil
}

// UpdateWorkspaceBranch stores the exact branch kenn-forge created
// for a workspace. Empty means setup reused a pre-existing local
// branch and therefore does not own it.
func (d *DB) UpdateWorkspaceBranch(
	ctx context.Context, id, branch string,
) error {
	result, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET workspace_branch = ?
		WHERE id = ?`,
		branch, id,
	)
	if err != nil {
		return fmt.Errorf("update workspace branch: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read workspace branch update result: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("workspace %q not found", id)
	}
	return nil
}

// CompleteRecoveredWorkspaceSetup publishes the recovered branch and ready
// status atomically so a failed persistence attempt leaves the recovery marker
// available to retry without cleaning the pre-existing worktree.
func (d *DB) CompleteRecoveredWorkspaceSetup(
	ctx context.Context, id, branch string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET workspace_branch = ?,
		    status = 'ready',
		    error_message = NULL
		WHERE id = ?`,
		branch, id,
	)
	if err != nil {
		return fmt.Errorf("complete recovered workspace setup: %w", err)
	}
	return nil
}

// UpdateWorkspaceMRHeadRepo persists a refreshed pull-request head-repo
// trust classification (nil for same-repo, empty string for unknown, or a
// clone URL for a fork) so a stale row from workspace creation cannot
// outlive a sync that reveals the true classification.
func (d *DB) UpdateWorkspaceMRHeadRepo(
	ctx context.Context, id string, mrHeadRepo *string,
) error {
	_, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET mr_head_repo = ?
		WHERE id = ?`,
		mrHeadRepo, id,
	)
	if err != nil {
		return fmt.Errorf("update workspace mr head repo: %w", err)
	}
	return nil
}

// UpdateWorkspaceMRHeadRepoForSnapshot persists a classification only while
// the merge-request revision and removed-upstream visibility that produced it
// remain current. A false result tells the caller to reread and retry.
func (d *DB) UpdateWorkspaceMRHeadRepoForSnapshot(
	ctx context.Context,
	id string,
	repoID int64,
	mrNumber int,
	expectedRevision int64,
	expectedRemoved bool,
	mrHeadRepo *string,
) (bool, error) {
	result, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET mr_head_repo = ?
		WHERE id = ?
		  AND repo_id = ?
		  AND COALESCE((
		      SELECT snapshot_revision
		      FROM forge_merge_requests
		      WHERE repo_id = ? AND number = ?
		  ), 0) = ?
		  AND EXISTS (
		      SELECT 1
		      FROM forge_archive_items
		      WHERE repo_id = ?
		        AND item_type = 'merge_request'
		        AND item_number = ?
		        AND lifecycle_state = 'removed_upstream'
		  ) = ?`,
		mrHeadRepo,
		id,
		repoID,
		repoID,
		mrNumber,
		expectedRevision,
		repoID,
		mrNumber,
		expectedRemoved,
	)
	if err != nil {
		return false, fmt.Errorf(
			"update workspace mr head repo for snapshot: %w", err,
		)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"read workspace mr head repo snapshot update result: %w", err,
		)
	}
	if rowsAffected > 0 {
		return true, nil
	}
	var workspaceExists bool
	if err := d.roQueryRowContext(
		ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM forge_workspaces WHERE id = ?
		)`,
		id,
	).Scan(&workspaceExists); err != nil {
		return false, fmt.Errorf(
			"check workspace after mr head repo snapshot update: %w", err,
		)
	}
	if !workspaceExists {
		return false, fmt.Errorf("workspace %q not found", id)
	}
	return false, nil
}

// StartWorkspaceRetry atomically transitions an errored workspace
// into setup state. It returns false when the workspace exists but
// was not in error status at the instant of the update.
func (d *DB) StartWorkspaceRetry(
	ctx context.Context, id string,
) (bool, error) {
	res, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET status = 'creating',
		    error_message = NULL
		WHERE id = ? AND status = 'error'`, id,
	)
	if err != nil {
		return false, fmt.Errorf("start workspace retry: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"start workspace retry rows affected: %w", err,
		)
	}
	return affected == 1, nil
}

// SetWorkspaceAssociatedPRNumberIfNull stores a workspace's first detected
// associated PR without overwriting an existing association.
func (d *DB) SetWorkspaceAssociatedPRNumberIfNull(
	ctx context.Context, id string, prNumber int,
) (bool, error) {
	res, err := d.execContext(ctx, `
		UPDATE forge_workspaces
		SET associated_pr_number = ?
		WHERE id = ? AND associated_pr_number IS NULL`,
		prNumber, id,
	)
	if err != nil {
		return false, fmt.Errorf(
			"set workspace associated PR number: %w", err,
		)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf(
			"set workspace associated PR number rows affected: %w", err,
		)
	}
	return rows > 0, nil
}

// InsertWorkspaceSetupEvent appends an audit event for workspace
// setup activity.
func (d *DB) InsertWorkspaceSetupEvent(
	ctx context.Context, event *WorkspaceSetupEvent,
) error {
	_, err := d.execContext(ctx, `
		INSERT INTO forge_workspace_setup_events
		    (workspace_id, stage, outcome, message)
		VALUES (?, ?, ?, ?)`,
		event.WorkspaceID, event.Stage, event.Outcome,
		event.Message,
	)
	if err != nil {
		return fmt.Errorf(
			"insert workspace setup event: %w", err,
		)
	}
	return nil
}

// ListWorkspaceSetupEvents returns the audit trail for a single
// workspace setup, ordered by insertion.
func (d *DB) ListWorkspaceSetupEvents(
	ctx context.Context, workspaceID string,
) ([]WorkspaceSetupEvent, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT id, workspace_id, stage, outcome, message,
		       created_at
		FROM forge_workspace_setup_events
		WHERE workspace_id = ?
		ORDER BY id`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"list workspace setup events: %w", err,
		)
	}
	defer rows.Close()

	var out []WorkspaceSetupEvent
	for rows.Next() {
		var event WorkspaceSetupEvent
		if err := rows.Scan(
			&event.ID, &event.WorkspaceID, &event.Stage,
			&event.Outcome, &event.Message, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf(
				"scan workspace setup event: %w", err,
			)
		}
		event.CreatedAt = event.CreatedAt.UTC()
		out = append(out, event)
	}
	return out, rows.Err()
}

// UpsertWorkspaceRuntimeSession records the public runtime session key and the
// metadata needed to restore or clean it up after a server restart.
func (d *DB) UpsertWorkspaceRuntimeSession(
	ctx context.Context,
	session *WorkspaceRuntimeSession,
) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		return upsertWorkspaceRuntimeSession(ctx, tx, session)
	})
}

// ListWorkspaceRuntimeSessions returns stored runtime sessions for a workspace
// ordered by creation time.
func (d *DB) ListWorkspaceRuntimeSessions(
	ctx context.Context,
	workspaceID string,
) ([]WorkspaceRuntimeSession, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT workspace_id, session_key, target_key, label, kind, display_region, scope,
		       tmux_session, created_at
		FROM forge_workspace_runtime_sessions
		WHERE workspace_id = ?
		ORDER BY created_at, session_key`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspace runtime sessions: %w", err)
	}
	defer rows.Close()

	return scanWorkspaceRuntimeSessions(rows)
}

// ListAllWorkspaceRuntimeSessions returns every stored runtime session. It is
// used during startup to rebuild in-memory attachments for durable backends.
func (d *DB) ListAllWorkspaceRuntimeSessions(
	ctx context.Context,
) ([]WorkspaceRuntimeSession, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT workspace_id, session_key, target_key, label, kind, display_region, scope,
		       tmux_session, created_at
		FROM forge_workspace_runtime_sessions
		ORDER BY workspace_id, created_at, session_key`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all workspace runtime sessions: %w", err)
	}
	defer rows.Close()

	return scanWorkspaceRuntimeSessions(rows)
}

// ListWorkspaceRuntimeTmuxSessions returns stored runtime sessions that own a
// tmux session for a workspace.
func (d *DB) ListWorkspaceRuntimeTmuxSessions(
	ctx context.Context,
	workspaceID string,
) ([]WorkspaceRuntimeSession, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT workspace_id, session_key, target_key, label, kind, display_region, scope,
		       tmux_session, created_at
		FROM forge_workspace_runtime_sessions
		WHERE workspace_id = ? AND tmux_session != ''
		ORDER BY created_at, session_key`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspace runtime tmux sessions: %w", err)
	}
	defer rows.Close()

	return scanWorkspaceRuntimeSessions(rows)
}

// ListAllWorkspaceRuntimeTmuxSessions returns every stored runtime session
// that owns a tmux session.
func (d *DB) ListAllWorkspaceRuntimeTmuxSessions(
	ctx context.Context,
) ([]WorkspaceRuntimeSession, error) {
	rows, err := d.roQueryContext(ctx, `
		SELECT workspace_id, session_key, target_key, label, kind, display_region, scope,
		       tmux_session, created_at
		FROM forge_workspace_runtime_sessions
		WHERE tmux_session != ''
		ORDER BY workspace_id, created_at, session_key`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all workspace runtime tmux sessions: %w", err)
	}
	defer rows.Close()

	return scanWorkspaceRuntimeSessions(rows)
}

// UpdateWorkspaceRuntimeSessionLabel updates the durable label metadata for a
// single runtime session.
func (d *DB) UpdateWorkspaceRuntimeSessionLabel(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	label string,
) (bool, error) {
	res, err := d.execContext(ctx, `
		UPDATE forge_workspace_runtime_sessions
		SET label = ?
		WHERE workspace_id = ? AND session_key = ?`,
		strings.TrimSpace(label), workspaceID, sessionKey,
	)
	if err != nil {
		return false, fmt.Errorf("update workspace runtime session label: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update workspace runtime session label rows: %w", err)
	}
	return rows > 0, nil
}

func scanWorkspaceRuntimeSessions(
	rows *sql.Rows,
) ([]WorkspaceRuntimeSession, error) {
	var out []WorkspaceRuntimeSession
	for rows.Next() {
		var session WorkspaceRuntimeSession
		if err := rows.Scan(
			&session.WorkspaceID, &session.SessionKey,
			&session.TargetKey, &session.Label, &session.Kind,
			&session.DisplayRegion, &session.Scope, &session.TmuxSession,
			&session.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workspace runtime session: %w", err)
		}
		session.CreatedAt = session.CreatedAt.UTC()
		out = append(out, session)
	}
	return out, rows.Err()
}

// DeleteWorkspaceRuntimeSession removes one stored runtime session.
func (d *DB) DeleteWorkspaceRuntimeSession(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
) error {
	_, err := d.execContext(ctx, `
		DELETE FROM forge_workspace_runtime_sessions
		WHERE workspace_id = ? AND session_key = ?`,
		workspaceID, sessionKey,
	)
	if err != nil {
		return fmt.Errorf("delete workspace runtime session: %w", err)
	}
	return nil
}

// DeleteWorkspaceRuntimeSessionCreatedAt removes one stored runtime session
// only if it still belongs to the same runtime session generation.
func (d *DB) DeleteWorkspaceRuntimeSessionCreatedAt(
	ctx context.Context,
	workspaceID string,
	sessionKey string,
	createdAt time.Time,
) (bool, error) {
	result, err := d.execContext(ctx, `
		DELETE FROM forge_workspace_runtime_sessions
		WHERE workspace_id = ? AND session_key = ? AND created_at = ?`,
		workspaceID, sessionKey, canonicalUTCTime(createdAt),
	)
	if err != nil {
		return false, fmt.Errorf("delete workspace runtime session: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete workspace runtime session rows: %w", err)
	}
	return rows > 0, nil
}

// DeleteWorkspaceRuntimeSessions removes every stored runtime session for a
// workspace.
func (d *DB) DeleteWorkspaceRuntimeSessions(
	ctx context.Context,
	workspaceID string,
) error {
	_, err := d.execContext(ctx, `
		DELETE FROM forge_workspace_runtime_sessions
		WHERE workspace_id = ?`, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("delete workspace runtime sessions: %w", err)
	}
	return nil
}

// DeleteWorkspace removes a workspace by ID.
func (d *DB) DeleteWorkspace(
	ctx context.Context, id string,
) error {
	_, err := d.execContext(ctx,
		`DELETE FROM forge_workspaces WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

// workspaceSummaryColumns is the SELECT list shared by
// ListWorkspaceSummaries and GetWorkspaceSummary.
const workspaceSummaryColumns = `
	w.id,
	COALESCE(r.platform, w.platform),
	COALESCE(r.platform_host, w.platform_host),
	COALESCE(r.owner, w.repo_owner),
	COALESCE(r.name, w.repo_name),
	w.repo_id,
	w.item_type, w.item_number, w.item_key, w.associated_pr_number,
	w.git_head_ref, w.mr_head_repo, w.workspace_branch,
	w.worktree_path, w.tmux_session, w.terminal_backend, w.status,
	w.error_message, w.created_at, w.kata_metadata,
	r.id, r.platform_repo_id,
	CASE
	    WHEN r.id IS NULL THEN 0
	    WHEN w.item_type = 'pull_request' THEN NOT EXISTS (
	        SELECT 1
	        FROM forge_archive_items source_a
	        WHERE source_a.repo_id = r.id
	          AND source_a.item_type = 'merge_request'
	          AND source_a.item_number = w.item_number
	          AND source_a.lifecycle_state = 'removed_upstream'
	    )
	    WHEN w.item_type = 'issue' THEN NOT EXISTS (
	        SELECT 1
	        FROM forge_archive_items source_a
	        WHERE source_a.repo_id = r.id
	          AND source_a.item_type = 'issue'
	          AND source_a.item_number = w.item_number
	          AND source_a.lifecycle_state = 'removed_upstream'
	    )
	    ELSE 1
	END,
	CASE
	    WHEN r.id IS NULL OR w.associated_pr_number IS NULL THEN 0
	    ELSE NOT EXISTS (
	        SELECT 1
	        FROM forge_archive_items associated_a
	        WHERE associated_a.repo_id = r.id
	          AND associated_a.item_type = 'merge_request'
	          AND associated_a.item_number = w.associated_pr_number
	          AND associated_a.lifecycle_state = 'removed_upstream'
	    )
	END,
	CASE
	    WHEN w.item_type = 'issue' THEN i.title
	    ELSE m.title
	END,
	CASE
	    WHEN w.item_type = 'issue' THEN i.state
	    ELSE m.state
	END,
	CASE
	    WHEN w.item_type = 'issue' THEN i.url
	    ELSE m.url
	END,
	m.is_draft, m.ci_status,
	m.review_decision, m.additions, m.deletions,
	m.comment_count, m.mergeable_state, m.head_branch,
	CASE
	    WHEN w.item_type = 'issue' THEN i.last_activity_at
	    ELSE m.last_activity_at
	END`

// workspaceSummaryJoins is the FROM/JOIN clause shared by
// ListWorkspaceSummaries and GetWorkspaceSummary.
const workspaceSummaryJoins = `
	FROM forge_workspaces w
	LEFT JOIN forge_repo_routes rr
	    ON w.repo_id IS NULL
	   AND rr.platform = w.platform
	   AND rr.platform_host = w.platform_host
	   AND rr.owner_key = w.repo_owner_key
	   AND rr.name_key = w.repo_name_key
	   AND NOT EXISTS (
	       SELECT 1
	       FROM forge_repo_routes historical
	       WHERE historical.platform = rr.platform
	         AND historical.platform_host = rr.platform_host
	         AND historical.repo_path_key = rr.repo_path_key
	         AND historical.repo_id <> rr.repo_id
	   )
	LEFT JOIN forge_repos r
	    ON r.id = COALESCE(w.repo_id, rr.repo_id)
	   AND r.lifecycle_state = 'active'
	LEFT JOIN forge_merge_requests m
	    ON m.repo_id = r.id
	   AND m.number = w.item_number
	   AND w.item_type = 'pull_request'
	   AND NOT EXISTS (
	       SELECT 1
	       FROM forge_archive_items a
	       WHERE a.repo_id = m.repo_id
	         AND a.item_type = 'merge_request'
	         AND a.item_number = m.number
	         AND a.lifecycle_state = 'removed_upstream'
	   )
	LEFT JOIN forge_issues i
	    ON i.repo_id = r.id
	   AND i.number = w.item_number
	   AND w.item_type = 'issue'
	   AND NOT EXISTS (
	       SELECT 1
	       FROM forge_archive_items a
	       WHERE a.repo_id = i.repo_id
	         AND a.item_type = 'issue'
	         AND a.item_number = i.number
	         AND a.lifecycle_state = 'removed_upstream'
	   )`

func scanWorkspaceSummary(
	scanner interface{ Scan(...any) error },
) (*WorkspaceSummary, error) {
	var s WorkspaceSummary
	var kataMetadataJSON string
	var itemLastActivityAt sql.NullString
	var workspaceRepoID sql.NullInt64
	var repoID sql.NullInt64
	var repoPlatformID sql.NullString
	err := scanner.Scan(
		&s.ID, &s.Platform, &s.PlatformHost, &s.RepoOwner, &s.RepoName,
		&workspaceRepoID,
		&s.ItemType, &s.ItemNumber, &s.ItemKey, &s.AssociatedPRNumber,
		&s.GitHeadRef, &s.MRHeadRepo, &s.WorkspaceBranch,
		&s.WorktreePath, &s.TmuxSession, &s.TerminalBackend, &s.Status,
		&s.ErrorMessage, &s.CreatedAt, &kataMetadataJSON,
		&repoID, &repoPlatformID,
		&s.SourceItemVisible, &s.AssociatedPRVisible,
		&s.SourceTitle, &s.SourceState, &s.SourceURL,
		&s.MRIsDraft, &s.MRCIStatus,
		&s.MRReviewDecision, &s.MRAdditions, &s.MRDeletions,
		&s.MRCommentCount, &s.MRMergeableState,
		&s.MRHeadBranch,
		&itemLastActivityAt,
	)
	if err != nil {
		return nil, err
	}
	if workspaceRepoID.Valid {
		s.Workspace.RepoID = workspaceRepoID.Int64
	}
	if repoID.Valid {
		s.RepoID = repoID.Int64
	}
	if repoPlatformID.Valid {
		s.RepoPlatformID = repoPlatformID.String
	}
	s.CreatedAt = s.CreatedAt.UTC()
	s.MRTitle = s.SourceTitle
	s.MRState = s.SourceState
	if s.ItemKey == "" && workspaceItemTypeKeysByNumber(s.ItemType) {
		s.ItemKey = strconv.Itoa(s.ItemNumber)
	}
	if strings.TrimSpace(kataMetadataJSON) != "" {
		var metadata WorkspaceKataMetadata
		if err := json.Unmarshal([]byte(kataMetadataJSON), &metadata); err != nil {
			return nil, fmt.Errorf("decode workspace kata metadata: %w", err)
		}
		s.KataMetadata = &metadata
		if s.ItemType == WorkspaceItemTypeKataTask && s.MRTitle == nil && metadata.Title != "" {
			title := metadata.Title
			s.MRTitle = &title
		}
	}
	if itemLastActivityAt.Valid {
		parsed, err := parseDBTime(itemLastActivityAt.String)
		if err != nil {
			return nil, err
		}
		utc := parsed.UTC()
		s.ItemLastActivityAt = &utc
	}
	return &s, nil
}

// ListWorkspaceSummaries returns all workspaces with joined MR
// metadata, ordered by created_at DESC.
func (d *DB) ListWorkspaceSummaries(
	ctx context.Context,
) ([]WorkspaceSummary, error) {
	query := "SELECT " + workspaceSummaryColumns +
		workspaceSummaryJoins +
		"\nORDER BY w.created_at DESC"
	rows, err := d.roQueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf(
			"list workspace summaries: %w", err,
		)
	}
	defer rows.Close()

	var out []WorkspaceSummary
	for rows.Next() {
		s, err := scanWorkspaceSummary(rows)
		if err != nil {
			return nil, fmt.Errorf(
				"scan workspace summary: %w", err,
			)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// GetWorkspaceSummary returns a single workspace with joined
// MR metadata, or nil if not found.
func (d *DB) GetWorkspaceSummary(
	ctx context.Context, id string,
) (*WorkspaceSummary, error) {
	query := "SELECT " + workspaceSummaryColumns +
		workspaceSummaryJoins +
		"\nWHERE w.id = ?"
	s, err := scanWorkspaceSummary(
		d.roQueryRowContext(ctx, query, id),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"get workspace summary: %w", err,
		)
	}
	return s, nil
}

func scanWorktreeLinks(
	rows *sql.Rows,
) ([]WorktreeLink, error) {
	var links []WorktreeLink
	for rows.Next() {
		var l WorktreeLink
		var path, branch sql.NullString
		var linkedAtStr string
		if err := rows.Scan(
			&l.ID, &l.MergeRequestID, &l.WorktreeKey,
			&path, &branch, &linkedAtStr,
		); err != nil {
			return nil, fmt.Errorf(
				"scan worktree link: %w", err,
			)
		}
		t, err := time.Parse(time.RFC3339, linkedAtStr)
		if err != nil {
			return nil, fmt.Errorf(
				"parse linked_at %q: %w", linkedAtStr, err,
			)
		}
		l.LinkedAt = t
		l.WorktreePath = path.String
		l.WorktreeBranch = branch.String
		links = append(links, l)
	}
	return links, rows.Err()
}
