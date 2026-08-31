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
