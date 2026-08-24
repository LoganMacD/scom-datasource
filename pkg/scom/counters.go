package scom

import (
	"context"
	"database/sql"
	"fmt"
)

// counterScopeClause builds an "AND EXISTS (...)" fragment that restricts a
// pri-aliased dbo.vPerformanceRuleInstance row to counters that actually
// have collected data for one of the given (already hosting-expanded)
// instance ids. Returns ("", nil) when instanceIDs is empty, applying no
// scope at all.
func counterScopeClause(instanceIDs []string) (string, []any) {
	if len(instanceIDs) == 0 {
		return "", nil
	}

	inSQL, inArgs := inClause("inst", instanceIDs)
	clause := fmt.Sprintf(`
	AND EXISTS (
		SELECT 1 FROM Perf.vPerfHourly ph
		INNER JOIN dbo.vManagedEntity me ON ph.ManagedEntityRowId = me.ManagedEntityRowId
		WHERE ph.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId
			AND me.ManagedEntityGuid IN %s
	)`, inSQL)
	return clause, inArgs
}

// SearchCounterObjects backs the /counter-objects resource endpoint: the
// first step of the Object -> Counter -> Instance picker. Returns distinct
// performance object names (e.g. "Process", "Memory"), scoped to instanceIDs
// like SearchCounterNames/SearchCounterInstances below.
func SearchCounterObjects(ctx context.Context, db *sql.DB, instanceIDs []string, search string) ([]Option, error) {
	scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{sql.Named("search", search)}, scopeArgs...)

	query := fmt.Sprintf(`
SELECT DISTINCT TOP %d
	pr.ObjectName AS Id,
	pr.ObjectName AS Label
FROM dbo.vPerformanceRule pr
INNER JOIN dbo.vPerformanceRuleInstance pri ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName LIKE @search + '%%'%s
ORDER BY pr.ObjectName`, defaultSearchLimit, scopeClause)

	return runOptionsQuery(ctx, db, query, args)
}

// SearchCounterNames backs the /counter-names resource endpoint: the second
// picker step, scoped to a chosen object. Returns distinct counter names
// (e.g. "Working Set", "% Processor Time") for that object.
func SearchCounterNames(ctx context.Context, db *sql.DB, instanceIDs []string, object, search string) ([]Option, error) {
	scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{sql.Named("search", search), sql.Named("object", object)}, scopeArgs...)

	query := fmt.Sprintf(`
SELECT DISTINCT TOP %d
	pr.CounterName AS Id,
	pr.CounterName AS Label
FROM dbo.vPerformanceRule pr
INNER JOIN dbo.vPerformanceRuleInstance pri ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object AND pr.CounterName LIKE @search + '%%'%s
ORDER BY pr.CounterName`, defaultSearchLimit, scopeClause)

	return runOptionsQuery(ctx, db, query, args)
}

// SearchCounterInstances backs the /counters resource endpoint: the third
// and final picker step, scoped to a chosen object + counter name. With
// nothing picked here, an object/counter selection alone means "every
// instance"; picking specific rows here narrows it down further. Returns the
// DW's own PerformanceRuleInstanceRowId as the option value — performance.go
// can then query Perf.vPerfHourly/Daily/Raw directly on that id.
func SearchCounterInstances(ctx context.Context, db *sql.DB, instanceIDs []string, object, counterName, search string) ([]Option, error) {
	scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{
		sql.Named("search", search),
		sql.Named("object", object),
		sql.Named("counterName", counterName),
	}, scopeArgs...)

	query := fmt.Sprintf(`
SELECT TOP %d
	CONVERT(varchar(20), pri.PerformanceRuleInstanceRowId) AS Id,
	CASE WHEN pri.InstanceName IS NOT NULL AND pri.InstanceName <> ''
		THEN pri.InstanceName ELSE '(all instances)' END AS Label
FROM dbo.vPerformanceRuleInstance pri
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object AND pr.CounterName = @counterName
	AND pri.InstanceName LIKE @search + '%%'%s
ORDER BY pri.InstanceName`, defaultSearchLimit, scopeClause)

	return runOptionsQuery(ctx, db, query, args)
}

// ResolveCounterInstanceIDs returns every PerformanceRuleInstanceRowId for
// the given object/counter name, scoped to instanceIDs (already
// hosting-expanded) if given, with no TOP cap and no free-text filter —
// unlike SearchCounterInstances, which is deliberately capped for the picker
// UI. It backs query execution for the case where an object/counter are
// chosen but no specific instances were narrowed down in the picker: "no
// instances narrowed" then means "every instance reporting this counter,"
// mirroring ResolveInstanceIDs's "no instances chosen" semantics.
func ResolveCounterInstanceIDs(ctx context.Context, db *sql.DB, instanceIDs []string, object, counterName string) ([]string, error) {
	scopeClause, scopeArgs := counterScopeClause(instanceIDs)
	args := append([]any{
		sql.Named("object", object),
		sql.Named("counterName", counterName),
	}, scopeArgs...)

	query := fmt.Sprintf(`
SELECT CONVERT(varchar(20), pri.PerformanceRuleInstanceRowId)
FROM dbo.vPerformanceRuleInstance pri
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object AND pr.CounterName = @counterName%s`, scopeClause)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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

func runOptionsQuery(ctx context.Context, db *sql.DB, query string, args []any) ([]Option, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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
