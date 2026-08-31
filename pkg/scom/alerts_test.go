package scom

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestBuildAlertsQuery(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("no filters still scopes by time range", func(t *testing.T) {
		query, args := buildAlertsQuery(AlertFilter{From: from, To: to})
		if !strings.Contains(query, "WHERE 1 = 1") {
			t.Errorf("query should start from an unconditional WHERE: %s", query)
		}
		if !strings.Contains(query, "a.TimeRaised >= @from AND a.TimeRaised <= @to") {
			t.Errorf("query missing time range filter: %s", query)
		}
		if !strings.Contains(query, "FROM dbo.Alert a") ||
			!strings.Contains(query, "INNER JOIN dbo.BaseManagedEntity bme ON a.BaseManagedEntityId = bme.BaseManagedEntityId") ||
			!strings.Contains(query, "LEFT JOIN dbo.ResolutionState rs ON rs.ResolutionState = a.ResolutionState") {
			t.Errorf("query missing expected table/joins: %s", query)
		}
		// Only the time range args when nothing else is filtered.
		if len(args) != 2 {
			t.Fatalf("got %d args, want 2 (from, to)", len(args))
		}
	})

	t.Run("instance/severity/resolution filters add IN clauses and args in order", func(t *testing.T) {
		query, args := buildAlertsQuery(AlertFilter{
			InstanceIDs:     []string{"inst-1"},
			Severities:      []int64{1, 2},
			ResolutionState: []int64{0},
			From:            from,
			To:              to,
		})
		if !strings.Contains(query, "CREATE TABLE #AlertScope") {
			t.Errorf("query missing instance scope temp table creation: %s", query)
		}
		if !strings.Contains(query, "INSERT INTO #AlertScope") {
			t.Errorf("query missing instance scope temp table population: %s", query)
		}
		if !strings.Contains(query, "AND a.BaseManagedEntityId IN (SELECT Id FROM #AlertScope)") {
			t.Errorf("query missing instance filter joined against the temp table: %s", query)
		}
		if !strings.Contains(query, "AND a.Severity IN (@sev0, @sev1)") {
			t.Errorf("query missing severity filter: %s", query)
		}
		if !strings.Contains(query, "AND a.ResolutionState IN (@res0)") {
			t.Errorf("query missing resolution state filter: %s", query)
		}

		// args must be built in the same order the WHERE fragments appear:
		// instance scope (always exactly one param, however many ids — see
		// idScopeTempTable), severity, resolution state, then from/to.
		wantNames := []string{"idScopeValues", "sev0", "sev1", "res0", "from", "to"}
		if len(args) != len(wantNames) {
			t.Fatalf("got %d args, want %d", len(args), len(wantNames))
		}
		for i, want := range wantNames {
			if got := args[i].(sql.NamedArg).Name; got != want {
				t.Errorf("arg %d: got name %q, want %q", i, got, want)
			}
		}
	})
}

func TestBuildAlertsWarehouseQuery(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("no filters still scopes by time range", func(t *testing.T) {
		query, args := buildAlertsWarehouseQuery(AlertFilter{From: from, To: to})
		if !strings.Contains(query, "WHERE 1 = 1") {
			t.Errorf("query should start from an unconditional WHERE: %s", query)
		}
		if !strings.Contains(query, "a.RaisedDateTime >= @from AND a.RaisedDateTime <= @to") {
			t.Errorf("query missing time range filter: %s", query)
		}
		if !strings.Contains(query, "FROM Alert.vAlert a") ||
			!strings.Contains(query, "INNER JOIN dbo.vManagedEntity me ON a.ManagedEntityRowId = me.ManagedEntityRowId") ||
			!strings.Contains(query, "LEFT JOIN LatestState ls ON ls.AlertGuid = a.AlertGuid AND ls.rn = 1") {
			t.Errorf("query missing expected table/joins: %s", query)
		}
		if len(args) != 2 {
			t.Fatalf("got %d args, want 2 (from, to)", len(args))
		}
	})

	t.Run("instance/severity/resolution filters add IN clauses and args in order", func(t *testing.T) {
		query, args := buildAlertsWarehouseQuery(AlertFilter{
			InstanceIDs:     []string{"inst-1"},
			Severities:      []int64{1, 2},
			ResolutionState: []int64{0},
			From:            from,
			To:              to,
		})
		if !strings.Contains(query, "CREATE TABLE #AlertScope") {
			t.Errorf("query missing instance scope temp table creation: %s", query)
		}
		if !strings.Contains(query, "INSERT INTO #AlertScope") {
			t.Errorf("query missing instance scope temp table population: %s", query)
		}
		if !strings.Contains(query, "AND me.ManagedEntityGuid IN (SELECT Id FROM #AlertScope)") {
			t.Errorf("query missing instance filter joined against the temp table: %s", query)
		}
		if !strings.Contains(query, "AND a.Severity IN (@sev0, @sev1)") {
			t.Errorf("query missing severity filter: %s", query)
		}
		if !strings.Contains(query, "AND ISNULL(ls.ResolutionState, 0) IN (@res0)") {
			t.Errorf("query missing resolution state filter: %s", query)
		}

		wantNames := []string{"idScopeValues", "sev0", "sev1", "res0", "from", "to"}
		if len(args) != len(wantNames) {
			t.Fatalf("got %d args, want %d", len(args), len(wantNames))
		}
		for i, want := range wantNames {
			if got := args[i].(sql.NamedArg).Name; got != want {
				t.Errorf("arg %d: got name %q, want %q", i, got, want)
			}
		}
	})
}
