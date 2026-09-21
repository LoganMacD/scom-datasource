package scom

import (
	"database/sql"
	"fmt"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// defaultSearchLimit caps how many rows the query editor's search pickers
// (classes/groups/instances/counters) return per keystroke. It's a single
// constant rather than a literal "TOP 50" duplicated across every picker
// query, so a deployment with unusually large class/group counts only needs
// this one line changed to search deeper.
const defaultSearchLimit = 50

// inClause builds a "(@prefix0, @prefix1, ...)" SQL fragment plus the
// matching sql.Named args for a variable-length list of values. Used so IN
// filters stay parameterized instead of string-concatenated.
func inClause(prefix string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}

	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, v := range values {
		name := fmt.Sprintf("%s%d", prefix, i)
		placeholders[i] = "@" + name
		args[i] = sql.Named(name, v)
	}

	return "(" + strings.Join(placeholders, ", ") + ")", args
}

// searchLikeEscape is the ESCAPE character every LIKE clause built with
// searchLikePattern must declare (via `LIKE @search ESCAPE '\'`), so the
// literal escaping below is actually honored by SQL Server rather than
// silently ignored.
const searchLikeEscape = `\`

// searchLikePattern turns free-text search input from the query editor into
// a SQL LIKE pattern that matches anywhere in the target column, not just as
// a prefix — so e.g. searching "cpu" finds "Processor - % CPU Time" — while
// letting a user type their own '*'/'?' wildcards for a more targeted
// pattern (e.g. "CPU*Time" or "Disk ? Read"), same as a shell glob. Any
// literal '\', '%', '_', or '[' already in the input is escaped first so it
// isn't misread as a SQL wildcard; only then are '*'/'?' translated to their
// SQL LIKE equivalents ('%'/'_') and the whole thing wrapped in '%...%' for
// the contains match. The wrap is harmless even when the user's own pattern
// already starts or ends with a wildcard, since '%' is idempotent next to
// another '%'.
func searchLikePattern(search string) string {
	var b strings.Builder
	b.Grow(len(search) + 2)
	b.WriteByte('%')
	for _, r := range search {
		switch r {
		case '\\', '%', '_', '[':
			b.WriteString(searchLikeEscape)
			b.WriteRune(r)
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('%')
	return b.String()
}

// localizedTextDisplayName is dbo.LocalizedText.LTStringType's value for an
// element's display name (2 is its description).
const localizedTextDisplayName = 1

// localizedNamePreference orders dbo.LocalizedText rows by which language
// this deployment would rather read a name in. ENU first because every
// sealed Microsoft management pack ships it and it's the one language
// guaranteed to name a monitor consistently; ENA next, which supplies the
// names ENU is missing; then anything else, so an MP localized into neither
// still gets a name rather than vanishing. LanguageCode breaks ties in that
// last bucket so the choice is deterministic rather than whatever order the
// engine happens to return.
const localizedNamePreference = `CASE lt.LanguageCode WHEN 'ENU' THEN 0 WHEN 'ENA' THEN 1 ELSE 2 END, lt.LanguageCode`

// localizedNameApply builds an OUTER APPLY that resolves one element's
// display name out of dbo.LocalizedText, aliased so the caller can select
// "<alias>.LTValue".
//
// This exists because SCOM's own *View wrappers (dbo.MonitorView,
// dbo.ManagedTypeView) LEFT JOIN LocalizedText with no language restriction
// at all, so they emit one row per installed language pack — every query
// through them has to collapse that somehow. Filtering to a single
// LanguageCode is the obvious way and the wrong one: it silently drops any
// element localized into some *other* language, and drops elements with no
// display string whatsoever, since their LanguageCode is NULL and NULL never
// equals anything. On a deployment running more than one English pack that's
// not hypothetical — an in-house MP authored under en-AU carries ENA strings
// and no ENU ones, and disappears entirely.
//
// TOP 1 collapses the duplication structurally instead, so language choice
// only decides which name is shown, never whether a row survives. Callers
// pair it with ISNULL(<alias>.LTValue, <the element's internal name>) to
// cover the no-display-string case, which is what the SCOM console shows
// there too.
func localizedNameApply(idColumn, alias string) string {
	return fmt.Sprintf(`
OUTER APPLY (
	SELECT TOP 1 lt.LTValue
	FROM dbo.LocalizedText lt
	WHERE lt.LTStringId = %s AND lt.LTStringType = %d
	ORDER BY %s
) %s`, idColumn, localizedTextDisplayName, localizedNamePreference, alias)
}

// monitorOperationalStateApply resolves a monitor's operator-facing state
// name for one health state, aliased "mos" so healthStateCase can read
// mos.MonitorOperationalStateName off it.
//
// An OUTER APPLY ... TOP 1 rather than the plain LEFT JOIN this replaced:
// nothing makes (MonitorId, HealthState) unique in
// dbo.MonitorOperationalState — a monitor may map several named operational
// states onto one health state — so a join on that pair fans out and
// multiplies whatever row it's attached to. Microsoft's own dbo.StateView
// joins it exactly that way and papers over the result with SELECT DISTINCT.
func monitorOperationalStateApply(monitorIDColumn, healthStateColumn string) string {
	return fmt.Sprintf(`
OUTER APPLY (
	SELECT TOP 1 mosi.MonitorOperationalStateName
	FROM dbo.MonitorOperationalState mosi
	WHERE mosi.MonitorId = %s AND mosi.HealthState = %s
	ORDER BY mosi.MonitorOperationalStateName
) mos`, monitorIDColumn, healthStateColumn)
}

// idScopeParamName is the fixed name of the single string parameter
// idScopeTempTable binds, regardless of how many ids are in the list.
const idScopeParamName = "idScopeValues"

// idScopeTempTable builds a "CREATE TABLE tableName ...; INSERT ...;" batch
// that populates a local temp table with values, plus the matching
// sql.Named arg. Used instead of inClause when a candidate id list can be
// large (e.g. a hosting-expanded class/group): folding hundreds or
// thousands of literal comparisons into an IN list inside an already
// multi-join query can make SQL Server's optimizer give up with "the query
// processor ran out of internal resources and could not produce a query
// plan." Joining against a temp table with real statistics instead keeps
// the plan simple — the same tradeoff ExpandHostedEntityIDs makes in
// relationships.go.
//
// The values are passed as a single comma-joined string bound to one
// @idScopeValues parameter and split server-side with STRING_SPLIT (every
// SQL Server version SCOM 2022 supports — 2016+ — has it), rather than one
// bound parameter per id. That distinction matters: SQL Server caps a
// single RPC at ~2,100 parameters total, and that cap applies to the whole
// call, not to any individual statement inside it — splitting the INSERTs
// into batches of, say, 1,000 rows each doesn't help, since all those
// parameters are still bound together in the one call. A single string
// parameter stays at exactly one regardless of list length.
//
// Bound as mssql.VarCharMax rather than a plain Go string: the driver
// always sends a bare string as NVARCHAR (UCS-2, 2 bytes/char), but this
// value is only ever a run of GUIDs and comma delimiters — pure ASCII, with
// nothing for the extra byte to buy. VarCharMax sends it as VARCHAR(MAX)
// instead, halving the parameter's wire size for a large list, with no
// functional difference: STRING_SPLIT's output is CONVERTed straight to
// uniqueidentifier below, and every join against it downstream compares
// real uniqueidentifier columns, never the string form. SELECT DISTINCT
// guards against a PRIMARY KEY violation if the same id appears twice in
// values. Returns ("", nil) when values is empty.
func idScopeTempTable(tableName string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}

	query := fmt.Sprintf(`
CREATE TABLE %s (Id uniqueidentifier PRIMARY KEY);
INSERT INTO %s (Id)
SELECT DISTINCT CONVERT(uniqueidentifier, value)
FROM STRING_SPLIT(@%s, ',');
`, tableName, tableName, idScopeParamName)

	return query, []any{sql.Named(idScopeParamName, mssql.VarCharMax(strings.Join(values, ",")))}
}
