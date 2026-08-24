package scom

import (
	"database/sql"
	"strings"
	"testing"
)

func TestCounterScopeClause(t *testing.T) {
	t.Run("empty instanceIDs applies no scope", func(t *testing.T) {
		clause, args := counterScopeClause(nil)
		if clause != "" || args != nil {
			t.Fatalf("got clause=%q args=%v, want empty", clause, args)
		}
	})

	t.Run("non-empty instanceIDs scopes by ManagedEntityGuid", func(t *testing.T) {
		clause, args := counterScopeClause([]string{"guid-1", "guid-2"})
		if !strings.Contains(clause, "AND EXISTS (") {
			t.Errorf("clause missing EXISTS wrapper: %s", clause)
		}
		if !strings.Contains(clause, "FROM Perf.vPerfHourly ph") {
			t.Errorf("clause missing Perf.vPerfHourly: %s", clause)
		}
		if !strings.Contains(clause, "me.ManagedEntityGuid IN") {
			t.Errorf("clause missing ManagedEntityGuid filter: %s", clause)
		}
		if !strings.Contains(clause, "ph.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId") {
			t.Errorf("clause missing correlation to outer pri alias: %s", clause)
		}
		if len(args) != 2 {
			t.Fatalf("got %d args, want 2", len(args))
		}
		if args[0].(sql.NamedArg).Value != "guid-1" || args[1].(sql.NamedArg).Value != "guid-2" {
			t.Errorf("got args=%v, want guid-1 then guid-2", args)
		}
	})
}
