package scom

import (
	"database/sql"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestSampleCounterScopeIDs(t *testing.T) {
	ids := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}
	if got := SampleCounterScopeIDs(ids); len(got) != CounterPickerSampleSize || got[9] != "10" {
		t.Errorf("got %v, want first %d ids", got, CounterPickerSampleSize)
	}
	if got := SampleCounterScopeIDs(ids[:3]); len(got) != 3 {
		t.Errorf("got %v, want ids unchanged", got)
	}
	if got := SampleCounterScopeIDs(nil); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestCounterScopeClause(t *testing.T) {
	t.Run("empty instanceIDs applies no scope", func(t *testing.T) {
		setupSQL, clause, args := counterScopeClause(nil)
		if setupSQL != "" || clause != "" || args != nil {
			t.Fatalf("got setupSQL=%q clause=%q args=%v, want empty", setupSQL, clause, args)
		}
	})

	t.Run("non-empty instanceIDs scopes via a temp table join", func(t *testing.T) {
		setupSQL, clause, args := counterScopeClause([]string{"guid-1", "guid-2"})
		if !strings.Contains(setupSQL, "CREATE TABLE #CounterScope") {
			t.Errorf("setupSQL missing temp table creation: %s", setupSQL)
		}
		if !strings.Contains(setupSQL, "INSERT INTO #CounterScope") {
			t.Errorf("setupSQL missing temp table population: %s", setupSQL)
		}
		if !strings.Contains(clause, "AND EXISTS (") {
			t.Errorf("clause missing EXISTS wrapper: %s", clause)
		}
		if !strings.Contains(clause, "FROM Perf.vPerfHourly ph") {
			t.Errorf("clause missing Perf.vPerfHourly: %s", clause)
		}
		if !strings.Contains(clause, "me.ManagedEntityGuid IN (SELECT Id FROM #CounterScope)") {
			t.Errorf("clause missing temp table join, still using a literal IN list: %s", clause)
		}
		if !strings.Contains(clause, "ph.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId") {
			t.Errorf("clause missing correlation to outer pri alias: %s", clause)
		}
		// Exactly one bound parameter regardless of how many ids are scoped —
		// see idScopeTempTable.
		if len(args) != 1 {
			t.Fatalf("got %d args, want 1", len(args))
		}
		if args[0].(sql.NamedArg).Value != mssql.VarCharMax("guid-1,guid-2") {
			t.Errorf("got args=%v, want a single comma-joined VarCharMax value", args)
		}
	})
}
