package scom

import (
	"database/sql"
	"fmt"
	"strings"
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

// idScopeTempTableChunkSize caps how many rows go into a single INSERT
// statement when populating an id-scope temp table, staying comfortably
// under SQL Server's 2,100-parameters-per-query-plan limit.
const idScopeTempTableChunkSize = 1000

// idScopeTempTable builds a "CREATE TABLE tableName ...; INSERT ...;" batch
// that populates a local temp table with values, plus the matching
// sql.Named args, split across chunked INSERT statements. Used instead of
// inClause when a candidate id list can be large (e.g. a hosting-expanded
// class/group): folding hundreds or thousands of literal comparisons into an
// IN list inside an already multi-join query can make SQL Server's optimizer
// give up with "the query processor ran out of internal resources and could
// not produce a query plan." Joining against a temp table with real
// statistics instead keeps the plan simple — the same tradeoff
// ExpandHostedEntityIDs makes in relationships.go. Returns ("", nil) when
// values is empty.
func idScopeTempTable(tableName string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (Id uniqueidentifier PRIMARY KEY);\n", tableName)

	var args []any
	for start := 0; start < len(values); start += idScopeTempTableChunkSize {
		end := start + idScopeTempTableChunkSize
		if end > len(values) {
			end = len(values)
		}
		chunk := values[start:end]

		rowSQL := make([]string, len(chunk))
		for i, v := range chunk {
			name := fmt.Sprintf("scope%d", start+i)
			rowSQL[i] = "(@" + name + ")"
			args = append(args, sql.Named(name, v))
		}
		fmt.Fprintf(&b, "INSERT INTO %s (Id) VALUES %s;\n", tableName, strings.Join(rowSQL, ", "))
	}

	return b.String(), args
}
