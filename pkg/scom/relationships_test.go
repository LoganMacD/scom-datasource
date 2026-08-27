package scom

import (
	"database/sql"
	"strings"
	"testing"
)

func TestExpandHostedEntityIDsQuery(t *testing.T) {
	t.Run("iterative expansion query avoids recursive CTE", func(t *testing.T) {
		query, args := expandHostedEntityIDsQuery([]string{"id-1", "id-2"})

		for _, want := range []string{
			"CREATE TABLE #HostingTypes",
			"CREATE TABLE #Visited",
			"CREATE TABLE #Frontier",
			"CREATE TABLE #NextFrontier",
			"WHILE @Depth < @MaxDepth",
			"INNER JOIN #Frontier f ON rel.SourceEntityId = f.Id",
			"NOT EXISTS (SELECT 1 FROM #Visited v",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}

		for _, avoid := range []string{";WITH Hosted", "MAXRECURSION"} {
			if strings.Contains(query, avoid) {
				t.Errorf("query should not contain %q\nfull query:\n%s", avoid, query)
			}
		}

		if len(args) != 2 {
			t.Fatalf("got %d args, want 2", len(args))
		}
		if args[0].(sql.NamedArg).Name != "root0" || args[1].(sql.NamedArg).Name != "root1" {
			t.Fatalf("unexpected arg names: %v", args)
		}
	})

	t.Run("empty ids still builds query with no IN args", func(t *testing.T) {
		query, args := expandHostedEntityIDsQuery(nil)
		if len(args) != 0 {
			t.Fatalf("got %d args, want 0", len(args))
		}
		if !strings.Contains(query, "#Frontier") {
			t.Fatalf("query missing expected traversal tables")
		}
	})
}
