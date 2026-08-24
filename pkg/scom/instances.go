package scom

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// InstanceFilter scopes an instance search to a class and/or a group.
// At least one of ClassID/GroupID is expected to be set by the query editor,
// but both are optional here so the same code path can serve a plain
// text-only browse if neither is chosen yet.
type InstanceFilter struct {
	ClassID string
	GroupID string
	Search  string
}

// scopeJoins builds the class/group scoping joins shared by SearchInstances
// (capped, search-filtered, for the picker) and ResolveInstanceIDs
// (uncapped, for query execution). Class scoping includes subclasses via
// dbo.DerivedManagedTypes (matching how groups.go resolves the class
// hierarchy); group scoping follows dbo.Relationship membership rows for the
// chosen group.
func scopeJoins(classID, groupID string) (string, []any) {
	var b strings.Builder
	var args []any

	if classID != "" {
		b.WriteString(`
INNER JOIN dbo.DerivedManagedTypes dmt ON bme.BaseManagedTypeId = dmt.DerivedTypeId
	AND dmt.BaseTypeId = @classId`)
		args = append(args, sql.Named("classId", classID))
	}

	if groupID != "" {
		b.WriteString(`
INNER JOIN dbo.Relationship rel ON rel.TargetEntityId = bme.BaseManagedEntityId
	AND rel.SourceEntityId = @groupId
	AND rel.IsDeleted = 0`)
		args = append(args, sql.Named("groupId", groupID))
	}

	return b.String(), args
}

// SearchInstances backs the /instances resource endpoint.
func SearchInstances(ctx context.Context, db *sql.DB, f InstanceFilter) ([]Option, error) {
	joins, joinArgs := scopeJoins(f.ClassID, f.GroupID)
	args := append([]any{sql.Named("search", f.Search)}, joinArgs...)

	query := fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS Id,
	bme.DisplayName
FROM dbo.BaseManagedEntity bme%s
WHERE bme.IsDeleted = 0
	AND bme.DisplayName LIKE @search + '%%'
ORDER BY bme.DisplayName`, defaultSearchLimit, joins)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Option
	for rows.Next() {
		var opt Option
		if err := rows.Scan(&opt.Value, &opt.Label); err != nil {
			return nil, err
		}
		out = append(out, opt)
	}
	return out, rows.Err()
}

// ResolveInstanceIDs returns every instance id in scope for a class and/or
// group, with no TOP cap and no free-text filter — unlike SearchInstances,
// which is deliberately capped for the picker UI. It backs query execution
// for the case where a class/group is chosen but no specific instances were
// picked: "no instances selected" then means "every instance in scope," not
// "no data." Returns (nil, nil) when neither classID nor groupID is set,
// since there's no scope to resolve "all instances" against.
func ResolveInstanceIDs(ctx context.Context, db *sql.DB, classID, groupID string) ([]string, error) {
	if classID == "" && groupID == "" {
		return nil, nil
	}

	joins, joinArgs := scopeJoins(classID, groupID)
	query := `
SELECT CONVERT(varchar(64), bme.BaseManagedEntityId)
FROM dbo.BaseManagedEntity bme` + joins + `
WHERE bme.IsDeleted = 0`

	rows, err := db.QueryContext(ctx, query, joinArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
