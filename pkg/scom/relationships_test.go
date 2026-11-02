package scom

import (
	"database/sql"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestExpandHostedEntityIDsQuery(t *testing.T) {
	t.Run("iterative expansion query avoids recursive CTE", func(t *testing.T) {
		query, args := expandHostedEntityIDsQuery([]string{"id-1", "id-2"})

		for _, want := range []string{
			"CREATE TABLE #RootIds",
			"INSERT INTO #RootIds",
			"CREATE TABLE #HostingTypes",
			"CREATE TABLE #Visited",
			"CREATE TABLE #Frontier",
			"CREATE TABLE #NextFrontier",
			"INNER JOIN #RootIds r ON r.Id = bme.BaseManagedEntityId",
			"WHILE @Depth < @MaxDepth",
			"INNER JOIN #Frontier f ON rel.SourceEntityId = f.Id",
			"NOT EXISTS (SELECT 1 FROM #Visited v",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}

		// The seed ids must be scoped via the #RootIds temp table join, not a
		// literal IN list — a class/group selection can be large enough to
		// blow past SQL Server's per-query parameter limit.
		for _, avoid := range []string{";WITH Hosted", "MAXRECURSION", "BaseManagedEntityId IN ("} {
			if strings.Contains(query, avoid) {
				t.Errorf("query should not contain %q\nfull query:\n%s", avoid, query)
			}
		}

		// Exactly one bound parameter regardless of how many seed ids there
		// are — see idScopeTempTable.
		if len(args) != 1 {
			t.Fatalf("got %d args, want 1", len(args))
		}
		named := args[0].(sql.NamedArg)
		if named.Name != "idScopeValues" {
			t.Fatalf("unexpected arg name: %v", named)
		}
		if named.Value != mssql.VarCharMax("id-1,id-2") {
			t.Fatalf("got arg value %v, want a single comma-joined VarCharMax value", named.Value)
		}
	})

	t.Run("empty ids still builds a valid query with no args", func(t *testing.T) {
		query, args := expandHostedEntityIDsQuery(nil)
		if len(args) != 0 {
			t.Fatalf("got %d args, want 0", len(args))
		}
		if !strings.Contains(query, "CREATE TABLE #RootIds") {
			t.Fatalf("query missing #RootIds temp table so the join stays valid with no seed rows")
		}
		if !strings.Contains(query, "#Frontier") {
			t.Fatalf("query missing expected traversal tables")
		}
	})
}

// A class/group scope resolves and expands in one batch, seeding #RootIds
// from the scope joins instead of from an id list the plugin sends back.
func TestExpandScopeIDsQuery(t *testing.T) {
	query, args := expandScopeIDsQuery("class-1", "group-1")

	for _, want := range []string{
		"CREATE TABLE #RootIds",
		"INSERT INTO #RootIds (Id)\nSELECT DISTINCT bme.BaseManagedEntityId",
		"dmt.BaseTypeId = @classId",
		"rel.SourceEntityId = @groupId",
		"WHERE bme.IsDeleted = 0;",
		// Then the same frontier expansion as ExpandHostedEntityIDs.
		"CREATE TABLE #HostingTypes",
		"WHILE @Depth < @MaxDepth",
		"FROM #Visited",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
	if strings.Contains(query, "STRING_SPLIT") {
		t.Errorf("query should seed from the scope joins, not a bound id list\nfull query:\n%s", query)
	}
	if len(args) != 2 {
		t.Fatalf("got %d args, want classId and groupId", len(args))
	}
}
