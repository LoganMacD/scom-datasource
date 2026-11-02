package scom

import (
	"context"
	"database/sql"
	"fmt"
)

// counterScopeTempTable is the fixed name of the local temp table
// counterScopeClause populates with instance ids. Fixed rather than derived
// per call, matching the #Visited/#Frontier naming in relationships.go — each
// call gets its own connection-scoped temp table, so there's no collision
// risk across concurrent requests.
const counterScopeTempTable = "#CounterScope"

// CounterPickerSampleSize is how many top-level instances the counter pickers
// look at when the selection is broad (a whole class/group, or more than this
// many explicit instances). Instances of one class report near-identical
// counters, so a small sample surfaces the same picker options at a fraction
// of the hosting-expansion and Perf.vPerfHourly scan cost.
const CounterPickerSampleSize = 10

// SampleCounterScopeIDs caps ids to the first CounterPickerSampleSize entries.
// It applies to the picker resource endpoints only; query execution still
// covers every selected instance.
func SampleCounterScopeIDs(ids []string) []string {
	if len(ids) > CounterPickerSampleSize {
		return ids[:CounterPickerSampleSize]
	}
	return ids
}

// counterScopeClause builds a setup batch plus an "AND EXISTS (...)"
// fragment that restricts a pri-aliased dbo.vPerformanceRuleInstance row to
// counters that actually have collected data for one of the given (already
// hosting-expanded) instance ids. The candidate ids are loaded into a temp
// table and joined via a subquery rather than folded into a large IN list —
// see idScopeTempTable for why. setupSQL must be prepended to the query
// text before existsClause is used. Returns ("", "", nil) when instanceIDs
// is empty, applying no scope at all.
func counterScopeClause(instanceIDs []string) (setupSQL, existsClause string, args []any) {
	setupSQL, args = idScopeTempTable(counterScopeTempTable, instanceIDs)
	if setupSQL == "" {
		return "", "", nil
	}

	existsClause = fmt.Sprintf(`
	AND EXISTS (
		SELECT 1 FROM Perf.vPerfHourly ph
		INNER JOIN dbo.vManagedEntity me ON ph.ManagedEntityRowId = me.ManagedEntityRowId
		WHERE ph.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId
			AND me.ManagedEntityGuid IN (SELECT Id FROM %s)
	)`, counterScopeTempTable)
	return setupSQL, existsClause, args
}

// counterObjectsQuery lists every performance object name (e.g. "Process",
// "Memory") within the scope. The list is cached per scope and searched in
// memory (see SearchCounterObjects), so typing in the picker doesn't rerun
// the Perf.vPerfHourly scan behind the scope clause.
func counterObjectsQuery(instanceIDs []string) (string, []any) {
	setupSQL, scopeClause, args := counterScopeClause(instanceIDs)
	return setupSQL + `
SELECT DISTINCT pr.ObjectName AS Id, pr.ObjectName AS Label
FROM dbo.vPerformanceRule pr
INNER JOIN dbo.vPerformanceRuleInstance pri ON pri.RuleRowId = pr.RuleRowId
WHERE 1 = 1` + scopeClause + `
ORDER BY pr.ObjectName`, args
}

// counterNamesQuery lists every counter name (e.g. "Working Set") of one
// object within the scope. Cached and searched like counterObjectsQuery.
func counterNamesQuery(instanceIDs []string, object string) (string, []any) {
	setupSQL, scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{sql.Named("object", object)}, scopeArgs...)
	return setupSQL + `
SELECT DISTINCT pr.CounterName AS Id, pr.CounterName AS Label
FROM dbo.vPerformanceRule pr
INNER JOIN dbo.vPerformanceRuleInstance pri ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object` + scopeClause + `
ORDER BY pr.CounterName`, args
}

// counterInstancesQuery searches the counter instances (e.g. "C:", "_Total")
// of one object + counter within the scope. Instance lists can be very long
// (every process on every server), so this one searches in SQL and returns
// at most defaultSearchLimit rows. The option value is the DW's own
// PerformanceRuleInstanceRowId, which performance.go queries on directly.
func counterInstancesQuery(instanceIDs []string, object, counterName, search string) (string, []any) {
	setupSQL, scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{
		sql.Named("search", searchLikePattern(search)),
		sql.Named("object", object),
		sql.Named("counterName", counterName),
	}, scopeArgs...)
	return setupSQL + fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(20), pri.PerformanceRuleInstanceRowId) AS Id,
	CASE WHEN pri.InstanceName IS NOT NULL AND pri.InstanceName <> ''
		THEN pri.InstanceName ELSE '(all instances)' END AS Label
FROM dbo.vPerformanceRuleInstance pri
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object AND pr.CounterName = @counterName
	AND pri.InstanceName LIKE @search ESCAPE '\'%s
ORDER BY pri.InstanceName`, defaultSearchLimit, scopeClause), args
}

// SearchCounterObjects backs the /counter-objects resource endpoint: the
// first step of the Object -> Counter -> Instance picker.
func SearchCounterObjects(ctx context.Context, db *DB, scope CounterScope, search string) ([]Option, error) {
	if scope.Scoped && len(scope.IDs) == 0 {
		return []Option{}, nil // the class/group has no instances
	}
	all, err := db.cache().options.get(ctx, cacheKey("counterObjects", scope.Key), metadataCacheTTL,
		func(ctx context.Context) ([]Option, error) {
			query, args := counterObjectsQuery(scope.IDs)
			return runOptionsQuery(ctx, db.Warehouse, query, args)
		})
	if err != nil {
		return nil, err
	}
	return searchOptions(all, search, defaultSearchLimit), nil
}

// SearchCounterNames backs the /counter-names resource endpoint: the second
// picker step, scoped to a chosen object.
func SearchCounterNames(ctx context.Context, db *DB, scope CounterScope, object, search string) ([]Option, error) {
	if scope.Scoped && len(scope.IDs) == 0 {
		return []Option{}, nil
	}
	all, err := db.cache().options.get(ctx, cacheKey("counterNames", scope.Key, object), metadataCacheTTL,
		func(ctx context.Context) ([]Option, error) {
			query, args := counterNamesQuery(scope.IDs, object)
			return runOptionsQuery(ctx, db.Warehouse, query, args)
		})
	if err != nil {
		return nil, err
	}
	return searchOptions(all, search, defaultSearchLimit), nil
}

// SearchCounterInstances backs the /counters resource endpoint: the third
// and final picker step, scoped to a chosen object + counter name. With
// nothing picked here, an object/counter selection alone means "every
// instance"; picking specific rows here narrows it down further.
func SearchCounterInstances(ctx context.Context, db *DB, scope CounterScope, object, counterName, search string) ([]Option, error) {
	if scope.Scoped && len(scope.IDs) == 0 {
		return []Option{}, nil
	}
	return db.cache().options.get(ctx, cacheKey("counterInstances", scope.Key, object, counterName, search), scopeCacheTTL,
		func(ctx context.Context) ([]Option, error) {
			query, args := counterInstancesQuery(scope.IDs, object, counterName, search)
			return runOptionsQuery(ctx, db.Warehouse, query, args)
		})
}

// runOptionsQuery runs a query returning (value, label) rows. It always
// returns a non-nil slice so the result marshals as [] rather than null.
func runOptionsQuery(ctx context.Context, db *sql.DB, query string, args []any) ([]Option, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Option{}
	for rows.Next() {
		var opt Option
		if err := rows.Scan(&opt.Value, &opt.Label); err != nil {
			return nil, err
		}
		out = append(out, opt)
	}
	return out, rows.Err()
}
