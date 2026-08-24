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
