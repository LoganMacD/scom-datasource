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
	args := append([]any{sql.Named("search", searchLikePattern(f.Search))}, joinArgs...)

	// DISTINCT: a group can relate to the same member more than once, which
	// would otherwise list it twice.
	query := fmt.Sprintf(`
SELECT DISTINCT TOP %d
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS Id,
	bme.DisplayName
FROM dbo.BaseManagedEntity bme%s
WHERE bme.IsDeleted = 0
	AND bme.DisplayName LIKE @search ESCAPE '\'
ORDER BY bme.DisplayName`, defaultSearchLimit, joins)

	return runOptionsQuery(ctx, db, query, args)
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

	query, args := resolveInstanceIDsQuery(classID, groupID)
	return queryStrings(ctx, db, query, args)
}

// resolveInstanceIDsQuery is ResolveInstanceIDs' query. DISTINCT because a
// group can relate to the same member through more than one relationship.
func resolveInstanceIDsQuery(classID, groupID string) (string, []any) {
	joins, args := scopeJoins(classID, groupID)
	return `
SELECT DISTINCT CONVERT(varchar(64), bme.BaseManagedEntityId)
FROM dbo.BaseManagedEntity bme` + joins + `
WHERE bme.IsDeleted = 0`, args
}

// variableValuesLimit caps how many instances a dashboard variable lists. A
// dropdown with more than this isn't usable, and the cap keeps one variable
// from pulling a whole huge class into the browser.
const variableValuesLimit = 10_000

// topScopeInstancesQuery lists the first limit instances in a class/group
// scope by display name: the counter pickers' sample (see
// CounterPickerSampleSize) and dashboard variable values. Sorted so the same
// scope always gives the same instances. limit is a constant, never input.
func topScopeInstancesQuery(classID, groupID string, limit int) (string, []any) {
	joins, args := scopeJoins(classID, groupID)
	return fmt.Sprintf(`
SELECT TOP (%d) CONVERT(varchar(64), x.BaseManagedEntityId) AS Id, x.DisplayName
FROM (
	SELECT DISTINCT bme.BaseManagedEntityId, bme.DisplayName
	FROM dbo.BaseManagedEntity bme%s
	WHERE bme.IsDeleted = 0
) x
ORDER BY x.DisplayName, x.BaseManagedEntityId`, limit, joins), args
}

// ListInstances returns up to variableValuesLimit instances in a class/group
// scope, for a dashboard variable.
func ListInstances(ctx context.Context, db *sql.DB, classID, groupID string) ([]Option, error) {
	if classID == "" && groupID == "" {
		return []Option{}, nil
	}
	query, args := topScopeInstancesQuery(classID, groupID, variableValuesLimit)
	return runOptionsQuery(ctx, db, query, args)
}

// sampleInstanceIDs returns the first n instance ids in a class/group scope.
func sampleInstanceIDs(ctx context.Context, db *sql.DB, classID, groupID string, n int) ([]string, error) {
	query, args := topScopeInstancesQuery(classID, groupID, n)
	opts, err := runOptionsQuery(ctx, db, query, args)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(opts))
	for i, o := range opts {
		ids[i] = o.Value
	}
	return ids, nil
}
