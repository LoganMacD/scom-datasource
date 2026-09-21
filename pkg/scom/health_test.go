package scom

import (
	"strings"
	"testing"
	"time"
)

// These assertions target the exact bugs found in production: dbo.Monitor
// has no DisplayName column, dbo.State has no InMaintenanceMode column, and
// dbo.StateChangeEvent's timestamp is TimeGenerated, not
// StateChangeEventTime. A test asserting the correct identifiers would have
// failed loudly on all three instead of surfacing as a runtime mssql error.
func TestBuildHealthCurrentQuery(t *testing.T) {
	query, args := buildHealthCurrentQuery([]string{"inst-1", "inst-2"})

	wantContain := []string{
		"FROM dbo.State s",
		"CONVERT(varchar(64), bme.BaseManagedEntityId) AS ManagedEntityId",
		"INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId",
		// Overall health only: scoped to the entity's health rollup monitor,
		// not joined against every component monitor.
		"AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()",
		// Health state display name, with the documented MonitorOperationalState
		// join and fallback CASE, not a raw HealthState int.
		// (MonitorId, HealthState) is not unique in
		// dbo.MonitorOperationalState, so the LEFT JOIN this replaced could
		// fan out and list an entity more than once — see
		// monitorOperationalStateApply.
		"SELECT TOP 1 mosi.MonitorOperationalStateName",
		"WHERE mosi.MonitorId = s.MonitorId AND mosi.HealthState = s.HealthState",
		"ISNULL(mos.MonitorOperationalStateName, CASE s.HealthState",
		// InMaintenanceMode comes from dbo.MaintenanceMode, not a nonexistent
		// column on dbo.State.
		"LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = bme.BaseManagedEntityId",
		"ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode",
		// Instance ids are scoped via a temp table join, not a literal IN
		// list — a raw, unexpanded class/group selection can be large enough
		// to blow past SQL Server's per-query parameter limit.
		"CREATE TABLE #HealthScope",
		"INSERT INTO #HealthScope",
		"WHERE s.BaseManagedEntityId IN (SELECT Id FROM #HealthScope)",
	}
	for _, want := range wantContain {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
	// The bug that shipped: a bare join to dbo.Monitor (no DisplayName column
	// there) instead of dbo.MonitorView/dbo.MonitorOperationalState.
	if strings.Contains(query, "dbo.Monitor m ") {
		t.Errorf("query should not join the bare dbo.Monitor table\nfull query:\n%s", query)
	}
	// Exactly one bound parameter regardless of how many instance ids are
	// scoped — see idScopeTempTable.
	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
}

func TestBuildHealthHistoryQuery(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	query, args := buildHealthHistoryQuery([]string{"inst-1"}, from, to)

	wantContain := []string{
		"FROM dbo.StateChangeEvent sce",
		"INNER JOIN dbo.State s ON sce.StateId = s.StateId",
		"AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()",
		"ISNULL(mosOld.MonitorOperationalStateName, CASE sce.OldHealthState",
		"ISNULL(mosNew.MonitorOperationalStateName, CASE sce.NewHealthState",
		// sce.TimeGenerated is the real column; StateChangeEventTime doesn't
		// exist on dbo.StateChangeEvent.
		"sce.TimeGenerated",
		"AND sce.TimeGenerated >= @from AND sce.TimeGenerated <= @to",
		"ORDER BY sce.TimeGenerated DESC",
		"CREATE TABLE #HealthScope",
		"WHERE s.BaseManagedEntityId IN (SELECT Id FROM #HealthScope)",
	}
	for _, want := range wantContain {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
	if strings.Contains(query, "StateChangeEventTime") {
		t.Errorf("query references the nonexistent StateChangeEventTime column\nfull query:\n%s", query)
	}
	if len(args) != 3 { // from, to, idScopeValues
		t.Fatalf("got %d args, want 3", len(args))
	}
}

func TestHealthTreeDrilldownLink(t *testing.T) {
	t.Run("no datasource UID means no link", func(t *testing.T) {
		if link := healthTreeDrilldownLink(""); link != nil {
			t.Fatalf("got link=%+v, want nil when datasourceUID is unset", link)
		}
	})

	t.Run("populated UID builds an Explore link targeting a health-tree query", func(t *testing.T) {
		link := healthTreeDrilldownLink("uid-1")
		if link == nil {
			t.Fatal("got nil link, want one")
		}
		if link.Internal == nil {
			t.Fatal("got no Internal link, want one targeting Explore")
		}
		if link.Internal.DatasourceUID != "uid-1" {
			t.Errorf("got DatasourceUID=%q, want %q", link.Internal.DatasourceUID, "uid-1")
		}
		query, ok := link.Internal.Query.(map[string]any)
		if !ok {
			t.Fatalf("Query is %T, want map[string]any", link.Internal.Query)
		}
		if query["queryType"] != string(QueryTypeHealthTree) {
			t.Errorf("got queryType=%v, want %q", query["queryType"], QueryTypeHealthTree)
		}
	})
}
