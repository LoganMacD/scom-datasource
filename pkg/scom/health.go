package scom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// healthStateCase renders a HealthState column as SCOM's operator-facing
// state name. dbo.MonitorOperationalState carries the per-monitor display
// name for a given (MonitorId, HealthState) pair, but — per the LEFT OUTER
// comment on Microsoft's own dbo.StateView — a monitor isn't guaranteed to
// have rows there, so this falls back to the well-known 0-3 health state
// codes (Not Monitored/Healthy/Warning/Critical) when it's missing.
//
// Reads off the "mos" alias that monitorOperationalStateApply supplies; that
// pairing is the whole contract, so the two are always used together.
func healthStateCase(monitorOSAlias, healthStateCol string) string {
	return fmt.Sprintf(`ISNULL(%s.MonitorOperationalStateName, CASE %s
		WHEN 0 THEN 'Not Monitored'
		WHEN 1 THEN 'Healthy'
		WHEN 2 THEN 'Warning'
		WHEN 3 THEN 'Critical'
		ELSE CONVERT(varchar(3), %s)
	END)`, monitorOSAlias, healthStateCol, healthStateCol)
}

// healthScopeTempTable is the temp table buildHealthCurrentQuery/
// buildHealthHistoryQuery populate with instanceIDs. Unlike the other
// query-scoping call sites, instanceIDs here can be the raw, unexpanded
// ResolveInstanceIDs output for a class/group (no ExpandHostedEntityIDs
// widening) — still large enough to blow past a literal IN list. See
// idScopeTempTable and the same fix in counters.go/performance.go/alerts.go.
const healthScopeTempTable = "#HealthScope"

// buildHealthCurrentQuery builds the query+args for QueryHealthCurrent. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see health_test.go.
func buildHealthCurrentQuery(instanceIDs []string) (string, []any) {
	setupSQL, args := idScopeTempTable(healthScopeTempTable, instanceIDs)
	query := setupSQL + fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS ManagedEntityId,
	bme.DisplayName AS ManagedEntity,
	%s AS HealthState,
	s.LastModified,
	ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode
FROM dbo.State s
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId%s
LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = bme.BaseManagedEntityId
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
ORDER BY bme.DisplayName`,
		healthStateCase("mos", "s.HealthState"),
		monitorOperationalStateApply("s.MonitorId", "s.HealthState"),
		healthScopeTempTable)
	return query, args
}

// healthTreeDrilldownLink builds a DataLink that opens Grafana Explore with
// a Health Tree query (QueryTypeHealthTree) pre-populated for one managed
// entity, so a Health query's results can drill straight into the same
// monitor tree SCOM's own Health Explorer shows for that object. The
// ${__data.fields.X} placeholders are filled in by Grafana at click time
// from the row the link came from — see the managedEntityId/managedEntity
// fields on the "health" frame below. datasourceUID is this data source
// instance's own UID (see Run), needed since the link targets this same
// data source; returns nil when it's unset (e.g. a test calling
// QueryHealthCurrent directly) rather than emit a link Explore would reject
// for having no target datasource.
func healthTreeDrilldownLink(datasourceUID string) *data.DataLink {
	if datasourceUID == "" {
		return nil
	}
	return &data.DataLink{
		Title: "View health tree",
		Internal: &data.InternalDataLink{
			DatasourceUID: datasourceUID,
			Query: map[string]any{
				"refId":     "A",
				"queryType": string(QueryTypeHealthTree),
				"instances": []map[string]string{{
					"value": "${__data.fields.managedEntityId}",
					"label": "${__data.fields.managedEntity}",
				}},
			},
		},
	}
}

// QueryHealthCurrent reads dbo.State on the Operational DB for the current
// overall health of the selected instances — the entity's health rollup
// monitor (System.Health.EntityState, looked up via
// dbo.fn_ManagedTypeId_SystemHealthEntityState()) rather than every
// individual component monitor underneath it. datasourceUID wires up the
// health tree drilldown link on the managedEntity field — see
// healthTreeDrilldownLink.
func QueryHealthCurrent(ctx context.Context, db *sql.DB, instanceIDs []string, datasourceUID string) (*data.Frame, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	query, args := buildHealthCurrentQuery(instanceIDs)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entityIDs, entities, healthStates []string
	var lastModified []time.Time
	var inMaintenance []bool

	for rows.Next() {
		var entityID, entity, healthState string
		var modified time.Time
		var maint sql.NullBool

		if err := rows.Scan(&entityID, &entity, &healthState, &modified, &maint); err != nil {
			return nil, err
		}
		entityIDs = append(entityIDs, entityID)
		entities = append(entities, entity)
		healthStates = append(healthStates, healthState)
		lastModified = append(lastModified, modified)
		inMaintenance = append(inMaintenance, maint.Bool)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	managedEntityField := data.NewField("managedEntity", nil, entities)
	if link := healthTreeDrilldownLink(datasourceUID); link != nil {
		managedEntityField.Config = &data.FieldConfig{Links: []data.DataLink{*link}}
	}

	frame := data.NewFrame("health",
		data.NewField("time", nil, lastModified),
		data.NewField("managedEntityId", nil, entityIDs),
		managedEntityField,
		data.NewField("healthState", nil, healthStates),
		data.NewField("inMaintenanceMode", nil, inMaintenance),
	)
	return frame, nil
}

// buildHealthHistoryQuery builds the query+args for QueryHealthHistory. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see health_test.go.
func buildHealthHistoryQuery(instanceIDs []string, from, to time.Time) (string, []any) {
	setupSQL, inArgs := idScopeTempTable(healthScopeTempTable, instanceIDs)
	args := append([]any{sql.Named("from", from), sql.Named("to", to)}, inArgs...)

	query := setupSQL + fmt.Sprintf(`
SELECT
	bme.DisplayName AS ManagedEntity,
	%s AS OldHealthState,
	%s AS NewHealthState,
	sce.TimeGenerated
FROM dbo.StateChangeEvent sce
INNER JOIN dbo.State s ON sce.StateId = s.StateId
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId
LEFT JOIN dbo.MonitorOperationalState mosOld ON mosOld.MonitorId = s.MonitorId AND mosOld.HealthState = sce.OldHealthState
LEFT JOIN dbo.MonitorOperationalState mosNew ON mosNew.MonitorId = s.MonitorId AND mosNew.HealthState = sce.NewHealthState
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
	AND sce.TimeGenerated >= @from AND sce.TimeGenerated <= @to
ORDER BY sce.TimeGenerated DESC`,
		healthStateCase("mosOld", "sce.OldHealthState"),
		healthStateCase("mosNew", "sce.NewHealthState"),
		healthScopeTempTable)
	return query, args
}

// QueryHealthHistory reads dbo.StateChangeEvent for overall health
// transitions — same System.Health.EntityState scoping as
// QueryHealthCurrent — on the selected instances within the panel's time
// range.
func QueryHealthHistory(ctx context.Context, db *sql.DB, instanceIDs []string, from, to time.Time) (*data.Frame, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	query, args := buildHealthHistoryQuery(instanceIDs, from, to)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var entities, oldStates, newStates []string
	var times []time.Time

	for rows.Next() {
		var entity, oldState, newState string
		var ts time.Time

		if err := rows.Scan(&entity, &oldState, &newState, &ts); err != nil {
			return nil, err
		}
		entities = append(entities, entity)
		oldStates = append(oldStates, oldState)
		newStates = append(newStates, newState)
		times = append(times, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := data.NewFrame("healthHistory",
		data.NewField("time", nil, times),
		data.NewField("managedEntity", nil, entities),
		data.NewField("oldHealthState", nil, oldStates),
		data.NewField("newHealthState", nil, newStates),
	)
	return frame, nil
}
