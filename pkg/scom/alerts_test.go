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

// The latest-state lookup only reads the history of the alerts in range;
// computed over the whole table, ROW_NUMBER ran across every state change the
// warehouse has ever kept.
func TestBuildAlertsWarehouseQueryLatestState(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	query, _ := buildAlertsWarehouseQuery(AlertFilter{From: from, To: to})
	for _, want := range []string{
		"INTO #AlertRows",
		"WHERE ars.AlertGuid IN (SELECT AlertGuid FROM #AlertRows)",
		"FROM #AlertRows a",
		// vManagedEntity's display name is nullable.
		"COALESCE(me.ManagedEntityDefaultName, me.Name, CONVERT(nvarchar(64), me.ManagedEntityGuid)) AS ManagedEntity",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
}

func TestBuildAlertsQueryTimeFilters(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("unset and raised keep the original raised-in-range filter", func(t *testing.T) {
		for _, filter := range []AlertTimeFilter{"", AlertTimeRaised} {
			query, args := buildAlertsQuery(AlertFilter{TimeFilter: filter, From: from, To: to})
			if !strings.Contains(query, "AND a.TimeRaised >= @from AND a.TimeRaised <= @to") {
				t.Errorf("filter %q: query missing raised-in-range filter:\n%s", filter, query)
			}
			if len(args) != 2 {
				t.Errorf("filter %q: got %d args, want 2", filter, len(args))
			}
		}
	})

	t.Run("active finds alerts open at any point in the range", func(t *testing.T) {
		query, args := buildAlertsQuery(AlertFilter{TimeFilter: AlertTimeActive, From: from, To: to})
		want := "AND ISNULL(a.TimeRaised, a.TimeAdded) <= @to AND (a.ResolutionState <> 255 OR a.TimeResolved >= @from)"
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
		if strings.Contains(query, "a.TimeRaised >= @from") {
			t.Errorf("active alerts raised before the range must not be filtered out:\n%s", query)
		}
		if len(args) != 2 {
			t.Errorf("got %d args, want 2", len(args))
		}
	})

	t.Run("ignore drops the time condition and its args", func(t *testing.T) {
		query, args := buildAlertsQuery(AlertFilter{TimeFilter: AlertTimeIgnore, From: from, To: to})
		if strings.Contains(query, "@from") || strings.Contains(query, "@to") {
			t.Errorf("query should have no time condition:\n%s", query)
		}
		if len(args) != 0 {
			t.Errorf("got %d args, want 0", len(args))
		}
	})

	// TimeRaised is nullable; once a NULL can get past the filter, the
	// selected time must not be NULL or scanning it fails.
	t.Run("selected time is never NULL", func(t *testing.T) {
		query, _ := buildAlertsQuery(AlertFilter{TimeFilter: AlertTimeIgnore})
		if !strings.Contains(query, "ISNULL(a.TimeRaised, a.TimeAdded) AS TimeRaised") {
			t.Errorf("query selects a nullable TimeRaised:\n%s", query)
		}
	})
}

func TestAlertsFrameStateColors(t *testing.T) {
	var rows alertRows
	rows.add("id", "Disk full", "server01", "Critical", "High", "New", "", "", 0, time.Time{}, time.Time{})

	plain := rows.frame(false)
	if f, _ := plain.FieldByName("severity"); f.Config != nil {
		t.Errorf("got severity config %+v without stateColors, want none", f.Config)
	}

	colored := rows.frame(true)
	f, _ := colored.FieldByName("severity")
	if f.Config == nil || len(f.Config.Mappings) == 0 {
		t.Fatal("stateColors set no severity mappings")
	}
}

func TestEmptyAlertsFrame(t *testing.T) {
	frame := emptyAlertsFrame(false)
	if frame.Rows() != 0 {
		t.Errorf("got %d rows, want 0", frame.Rows())
	}
	if len(frame.Fields) != 11 {
		t.Errorf("got %d fields, want the usual alert columns", len(frame.Fields))
	}
	if frame.Meta == nil || len(frame.Meta.Notices) != 1 {
		t.Errorf("got meta %+v, want one notice explaining the empty scope", frame.Meta)
	}
}
