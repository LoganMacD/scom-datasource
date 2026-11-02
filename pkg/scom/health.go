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
// Reads off the alias that monitorOperationalStateApply supplies; that
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

// healthScopeTempTable is the temp table the health queries populate with
// instanceIDs. Unlike the other query-scoping call sites, instanceIDs here
// can be the raw, unexpanded ResolveInstanceIDs output for a class/group (no
// ExpandHostedEntityIDs widening) — still large enough to blow past a
// literal IN list. See idScopeTempTable and the same fix in
// counters.go/performance.go/alerts.go.
const healthScopeTempTable = "#HealthScope"

// buildHealthCurrentQuery builds the query+args for QueryHealthCurrent. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see health_test.go. HealthStateCode is the raw
// state, read only to color the state name (see QueryHealthCurrent).
func buildHealthCurrentQuery(instanceIDs []string) (string, []any) {
	setupSQL, args := idScopeTempTable(healthScopeTempTable, instanceIDs)
	query := setupSQL + fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), bme.BaseManagedEntityId) AS ManagedEntityId,
	bme.DisplayName AS ManagedEntity,
	%s AS HealthState,
	s.HealthState AS HealthStateCode,
	s.LastModified,
	ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode
FROM dbo.State s
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId%s
LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = bme.BaseManagedEntityId
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
ORDER BY bme.DisplayName`,
		healthStateCase("mos", "s.HealthState"),
		monitorOperationalStateApply("mos", "s.MonitorId", "s.HealthState"),
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
// healthTreeDrilldownLink. stateColors colors the health state and shows
// maintenance mode as Yes/No (see colorStateField).
func QueryHealthCurrent(ctx context.Context, db *sql.DB, instanceIDs []string, datasourceUID string, stateColors bool) (*data.Frame, error) {
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
	colors := healthNameColors{}

	for rows.Next() {
		var entityID, entity, healthState string
		var code int64
		var modified time.Time
		var maint sql.NullBool

		if err := rows.Scan(&entityID, &entity, &healthState, &code, &modified, &maint); err != nil {
			return nil, err
		}
		entityIDs = append(entityIDs, entityID)
		entities = append(entities, entity)
		healthStates = append(healthStates, healthState)
		lastModified = append(lastModified, modified)
		inMaintenance = append(inMaintenance, maint.Bool)
		colors.add(healthState, code)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	managedEntityField := data.NewField("managedEntity", nil, entities)
	if link := healthTreeDrilldownLink(datasourceUID); link != nil {
		managedEntityField.Config = &data.FieldConfig{Links: []data.DataLink{*link}}
	}
	healthStateField := data.NewField("healthState", nil, healthStates)
	maintenanceField := data.NewField("inMaintenanceMode", nil, inMaintenance)
	if stateColors {
		colorStateField(healthStateField, valueColors(colors).mappings())
		colorStateField(maintenanceField, maintenanceModeMappings())
	}

	frame := data.NewFrame("health",
		data.NewField("time", nil, lastModified),
		data.NewField("managedEntityId", nil, entityIDs),
		managedEntityField,
		healthStateField,
		maintenanceField,
	)
	return frame, nil
}

// buildHealthHistoryQuery builds the query+args for QueryHealthHistory. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see health_test.go. Each state name comes from
// its own monitorOperationalStateApply, so a monitor mapping several names
// onto one health state can't duplicate a transition.
func buildHealthHistoryQuery(instanceIDs []string, from, to time.Time) (string, []any) {
	setupSQL, inArgs := idScopeTempTable(healthScopeTempTable, instanceIDs)
	args := append([]any{sql.Named("from", from), sql.Named("to", to)}, inArgs...)

	query := setupSQL + fmt.Sprintf(`
SELECT
	bme.DisplayName AS ManagedEntity,
	%s AS OldHealthState,
	%s AS NewHealthState,
	sce.OldHealthState AS OldHealthStateCode,
	sce.NewHealthState AS NewHealthStateCode,
	sce.TimeGenerated
FROM dbo.StateChangeEvent sce
INNER JOIN dbo.State s ON sce.StateId = s.StateId
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId%s%s
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
	AND sce.TimeGenerated >= @from AND sce.TimeGenerated <= @to
ORDER BY sce.TimeGenerated DESC`,
		healthStateCase("mosOld", "sce.OldHealthState"),
		healthStateCase("mosNew", "sce.NewHealthState"),
		monitorOperationalStateApply("mosOld", "s.MonitorId", "sce.OldHealthState"),
		monitorOperationalStateApply("mosNew", "s.MonitorId", "sce.NewHealthState"),
		healthScopeTempTable)
	return query, args
}

// QueryHealthHistory reads dbo.StateChangeEvent for overall health
// transitions — same System.Health.EntityState scoping as
// QueryHealthCurrent — on the selected instances within the panel's time
// range.
func QueryHealthHistory(ctx context.Context, db *sql.DB, instanceIDs []string, from, to time.Time, stateColors bool) (*data.Frame, error) {
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
	colors := healthNameColors{}

	for rows.Next() {
		var entity, oldState, newState string
		var oldCode, newCode int64
		var ts time.Time

		if err := rows.Scan(&entity, &oldState, &newState, &oldCode, &newCode, &ts); err != nil {
			return nil, err
		}
		entities = append(entities, entity)
		oldStates = append(oldStates, oldState)
		newStates = append(newStates, newState)
		times = append(times, ts)
		colors.add(oldState, oldCode)
		colors.add(newState, newCode)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	oldField := data.NewField("oldHealthState", nil, oldStates)
	newField := data.NewField("newHealthState", nil, newStates)
	if stateColors {
		mappings := valueColors(colors).mappings()
		colorStateField(oldField, mappings)
		colorStateField(newField, mappings)
	}
	frame := data.NewFrame("healthHistory",
		data.NewField("time", nil, times),
		data.NewField("managedEntity", nil, entities),
		oldField,
		newField,
	)
	return frame, nil
}

// buildHealthTimelineQuery builds the query+args for QueryHealthTimeline: for
// each entity, its state at the start of the range (Kind 0, stamped @from),
// then every change inside the range (Kind 1). Same System.Health.EntityState
// scoping as QueryHealthCurrent.
//
// The starting state is the new state of the last change before the range;
// failing that (the Operational DB grooms state changes after a few days),
// the old state of the first change at or after the start; failing that, the
// entity has had no recorded change at all and its current state applies.
func buildHealthTimelineQuery(instanceIDs []string, from, to time.Time) (string, []any) {
	setupSQL, inArgs := idScopeTempTable(healthScopeTempTable, instanceIDs)
	args := append([]any{sql.Named("from", from), sql.Named("to", to)}, inArgs...)

	query := setupSQL + fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), s.BaseManagedEntityId) AS ManagedEntityId,
	bme.DisplayName AS ManagedEntity,
	CONVERT(datetime, @from) AS TimeGenerated,
	CONVERT(int, COALESCE(lastBefore.NewHealthState, firstAfter.OldHealthState, s.HealthState)) AS HealthState,
	0 AS Kind
FROM dbo.State s
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId
OUTER APPLY (
	SELECT TOP 1 sceb.NewHealthState
	FROM dbo.StateChangeEvent sceb
	WHERE sceb.StateId = s.StateId AND sceb.TimeGenerated < @from
	ORDER BY sceb.TimeGenerated DESC
) lastBefore
OUTER APPLY (
	SELECT TOP 1 scea.OldHealthState
	FROM dbo.StateChangeEvent scea
	WHERE scea.StateId = s.StateId AND scea.TimeGenerated >= @from
	ORDER BY scea.TimeGenerated
) firstAfter
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %[1]s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
UNION ALL
SELECT
	CONVERT(varchar(64), s.BaseManagedEntityId),
	bme.DisplayName,
	sce.TimeGenerated,
	CONVERT(int, sce.NewHealthState),
	1
FROM dbo.StateChangeEvent sce
INNER JOIN dbo.State s ON sce.StateId = s.StateId
INNER JOIN dbo.BaseManagedEntity bme ON s.BaseManagedEntityId = bme.BaseManagedEntityId
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %[1]s)
	AND s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()
	AND sce.TimeGenerated >= @from AND sce.TimeGenerated <= @to
ORDER BY ManagedEntity, ManagedEntityId, TimeGenerated, Kind`, healthScopeTempTable)
	return query, args
}

// healthTimelineRow is one scanned row from buildHealthTimelineQuery.
type healthTimelineRow struct {
	EntityID string
	Entity   string
	Time     time.Time
	State    int64
}

// healthTimelineFrames turns timeline rows (ordered by entity, then time) into
// one frame per entity: numeric health states with value mappings that name
// and color them, which is what a State timeline panel draws. Each series
// ends with a point at end carrying its last state, so the bar reaches the
// end of the range rather than stopping at the last change.
func healthTimelineFrames(rows []healthTimelineRow, end time.Time) []*data.Frame {
	var frames []*data.Frame
	for i := 0; i < len(rows); {
		j := i
		for j < len(rows) && rows[j].EntityID == rows[i].EntityID {
			j++
		}
		group := rows[i:j]
		i = j

		times := make([]time.Time, 0, len(group)+1)
		states := make([]int64, 0, len(group)+1)
		for _, r := range group {
			times = append(times, r.Time)
			states = append(states, r.State)
		}
		if end.After(times[len(times)-1]) {
			times = append(times, end)
			states = append(states, states[len(states)-1])
		}

		entity := group[0].Entity
		field := data.NewField("healthState", data.Labels{"entity": entity, "entityId": group[0].EntityID}, states)
		field.Config = &data.FieldConfig{DisplayNameFromDS: entity, Mappings: healthCodeMappings()}
		frame := data.NewFrame(entity, data.NewField("time", nil, times), field)
		frame.Meta = &data.FrameMeta{
			Type:        data.FrameTypeTimeSeriesMulti,
			TypeVersion: data.FrameTypeVersion{0, 1},
		}
		frames = append(frames, frame)
	}
	return frames
}

// QueryHealthTimeline returns each selected entity's overall health over the
// time range, for a State timeline panel — see buildHealthTimelineQuery and
// healthTimelineFrames.
func QueryHealthTimeline(ctx context.Context, db *sql.DB, instanceIDs []string, from, to time.Time) ([]*data.Frame, error) {
	if len(instanceIDs) == 0 {
		return nil, nil
	}

	query, args := buildHealthTimelineQuery(instanceIDs, from, to)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var timeline []healthTimelineRow
	for rows.Next() {
		var r healthTimelineRow
		var kind int64
		if err := rows.Scan(&r.EntityID, &r.Entity, &r.Time, &r.State, &kind); err != nil {
			return nil, err
		}
		timeline = append(timeline, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	end := to
	if now := time.Now().UTC(); now.Before(end) {
		end = now
	}
	return healthTimelineFrames(timeline, end), nil
}
