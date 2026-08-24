package scom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

type AlertFilter struct {
	InstanceIDs     []string
	Severities      []int64 // 0=Information, 1=Warning, 2=Critical
	ResolutionState []int64 // 0=New, 255=Closed, others MP/user-defined
	From, To        time.Time
}

// ListResolutionStates backs the /resolution-states resource endpoint used
// by the query editor's alert filter. dbo.ResolutionState holds both SCOM's
// four built-in states (New/Acknowledged/Resolved/Closed) and any
// custom ones an administrator has added (Operations Console > Administration
// > Alert Resolution States), so this can't be a fixed list on the frontend.
func ListResolutionStates(ctx context.Context, db *sql.DB) ([]Option, error) {
	rows, err := db.QueryContext(ctx, `
SELECT CONVERT(varchar(3), ResolutionState), ResolutionStateName
FROM dbo.ResolutionState
ORDER BY ResolutionState`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Option
	for rows.Next() {
		var opt Option
		if err := rows.Scan(&opt.Value, &opt.Label); err != nil {
			return nil, err
		}
		out = append(out, opt)
	}
	return out, rows.Err()
}

// buildAlertsQuery builds the query+args for QueryAlerts. Kept separate from
// execution so the generated SQL/WHERE clause can be asserted against in
// tests without a live DB — see alerts_test.go.
func buildAlertsQuery(f AlertFilter) (string, []any) {
	var args []any
	where := "1 = 1"

	if len(f.InstanceIDs) > 0 {
		inSQL, inArgs := inClause("inst", f.InstanceIDs)
		where += fmt.Sprintf(" AND a.BaseManagedEntityId IN %s", inSQL)
		args = append(args, inArgs...)
	}
	if len(f.Severities) > 0 {
		strs := make([]string, len(f.Severities))
		for i, s := range f.Severities {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("sev", strs)
		where += fmt.Sprintf(" AND a.Severity IN %s", inSQL)
		args = append(args, inArgs...)
	}
	if len(f.ResolutionState) > 0 {
		strs := make([]string, len(f.ResolutionState))
		for i, s := range f.ResolutionState {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("res", strs)
		where += fmt.Sprintf(" AND a.ResolutionState IN %s", inSQL)
		args = append(args, inArgs...)
	}

	where += " AND a.TimeRaised >= @from AND a.TimeRaised <= @to"
	args = append(args, sql.Named("from", f.From), sql.Named("to", f.To))

	query := fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), a.AlertId) AS AlertId,
	a.AlertName,
	bme.DisplayName AS ManagedEntity,
	CASE a.Severity WHEN 0 THEN 'Information' WHEN 1 THEN 'Warning' WHEN 2 THEN 'Critical' ELSE CONVERT(varchar(3), a.Severity) END AS Severity,
	CASE a.Priority WHEN 0 THEN 'Low' WHEN 1 THEN 'Normal' WHEN 2 THEN 'High' ELSE CONVERT(varchar(3), a.Priority) END AS Priority,
	ISNULL(rs.ResolutionStateName, CONVERT(varchar(3), a.ResolutionState)) AS ResolutionState,
	a.RepeatCount,
	a.Owner,
	a.TimeRaised,
	a.LastModified,
	a.AlertDescription
FROM dbo.Alert a
INNER JOIN dbo.BaseManagedEntity bme ON a.BaseManagedEntityId = bme.BaseManagedEntityId
LEFT JOIN dbo.ResolutionState rs ON rs.ResolutionState = a.ResolutionState
WHERE %s
ORDER BY a.TimeRaised DESC`, where)

	return query, args
}

// QueryAlerts reads dbo.Alert on the Operational DB, filtered to alerts
// raised for the selected instances within the panel's time range.
func QueryAlerts(ctx context.Context, db *sql.DB, f AlertFilter) (*data.Frame, error) {
	query, args := buildAlertsQuery(f)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var (
		alertIDs, names, entities, severities, priorities, resolutionStates, owners, descriptions []string
		repeatCounts                                                                              []int64
		timeRaised, lastModified                                                                  []time.Time
	)

	for rows.Next() {
		var alertID, name, entity, severity, priority, resolutionState string
		var repeatCount int64
		var owner, description sql.NullString
		var raised, modified time.Time

		if err := rows.Scan(&alertID, &name, &entity, &severity, &priority, &resolutionState, &repeatCount, &owner, &raised, &modified, &description); err != nil {
			return nil, err
		}

		alertIDs = append(alertIDs, alertID)
		names = append(names, name)
		entities = append(entities, entity)
		severities = append(severities, severity)
		priorities = append(priorities, priority)
		resolutionStates = append(resolutionStates, resolutionState)
		repeatCounts = append(repeatCounts, repeatCount)
		owners = append(owners, owner.String)
		descriptions = append(descriptions, description.String)
		timeRaised = append(timeRaised, raised)
		lastModified = append(lastModified, modified)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := data.NewFrame("alerts",
		data.NewField("time", nil, timeRaised),
		data.NewField("alertId", nil, alertIDs),
		data.NewField("name", nil, names),
		data.NewField("managedEntity", nil, entities),
		data.NewField("severity", nil, severities),
		data.NewField("priority", nil, priorities),
		data.NewField("resolutionState", nil, resolutionStates),
		data.NewField("repeatCount", nil, repeatCounts),
		data.NewField("owner", nil, owners),
		data.NewField("lastModified", nil, lastModified),
		data.NewField("description", nil, descriptions),
	)
	return frame, nil
}

// buildAlertsWarehouseQuery builds the query+args for QueryAlertsWarehouse.
// Kept separate from execution so the generated SQL/WHERE clause can be
// asserted against in tests without a live DB — see alerts_test.go.
//
// Alert.vAlert has no direct "current resolution state" column — only
// Alert.vAlertResolutionState, a history of every state change — so the
// LatestState CTE below picks the most recent row per alert via ROW_NUMBER.
// A brand new alert that hasn't had a state row written yet falls back to
// resolution state 0 (New) via ISNULL, matching dbo.Alert's own default.
func buildAlertsWarehouseQuery(f AlertFilter) (string, []any) {
	var args []any
	where := "1 = 1"

	if len(f.InstanceIDs) > 0 {
		inSQL, inArgs := inClause("inst", f.InstanceIDs)
		where += fmt.Sprintf(" AND me.ManagedEntityGuid IN %s", inSQL)
		args = append(args, inArgs...)
	}
	if len(f.Severities) > 0 {
		strs := make([]string, len(f.Severities))
		for i, s := range f.Severities {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("sev", strs)
		where += fmt.Sprintf(" AND a.Severity IN %s", inSQL)
		args = append(args, inArgs...)
	}
	if len(f.ResolutionState) > 0 {
		strs := make([]string, len(f.ResolutionState))
		for i, s := range f.ResolutionState {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("res", strs)
		where += fmt.Sprintf(" AND ISNULL(ls.ResolutionState, 0) IN %s", inSQL)
		args = append(args, inArgs...)
	}

	where += " AND a.RaisedDateTime >= @from AND a.RaisedDateTime <= @to"
	args = append(args, sql.Named("from", f.From), sql.Named("to", f.To))

	query := fmt.Sprintf(`
;WITH LatestState AS (
	SELECT
		ars.AlertGuid,
		ars.ResolutionState,
		ars.StateSetDateTime,
		ROW_NUMBER() OVER (PARTITION BY ars.AlertGuid ORDER BY ars.StateSetDateTime DESC) AS rn
	FROM Alert.vAlertResolutionState ars
)
SELECT
	CONVERT(varchar(64), a.AlertGuid) AS AlertId,
	a.AlertName,
	me.ManagedEntityDefaultName AS ManagedEntity,
	CASE a.Severity WHEN 0 THEN 'Information' WHEN 1 THEN 'Warning' WHEN 2 THEN 'Critical' ELSE CONVERT(varchar(3), a.Severity) END AS Severity,
	CASE a.Priority WHEN 0 THEN 'Low' WHEN 1 THEN 'Normal' WHEN 2 THEN 'High' ELSE CONVERT(varchar(3), a.Priority) END AS Priority,
	CONVERT(varchar(3), ISNULL(ls.ResolutionState, 0)) AS ResolutionState,
	a.RepeatCount,
	a.RaisedDateTime,
	ISNULL(ls.StateSetDateTime, a.RaisedDateTime) AS LastModified,
	a.AlertDescription
FROM Alert.vAlert a
INNER JOIN dbo.vManagedEntity me ON a.ManagedEntityRowId = me.ManagedEntityRowId
LEFT JOIN LatestState ls ON ls.AlertGuid = a.AlertGuid AND ls.rn = 1
WHERE %s
ORDER BY a.RaisedDateTime DESC`, where)

	return query, args
}

// resolutionStateNameMap looks up dbo.ResolutionState on the given
// connection and returns it as a code (matching ListResolutionStates'
// CONVERT(varchar(3), ResolutionState) formatting) -> name map.
func resolutionStateNameMap(ctx context.Context, db *sql.DB) (map[string]string, error) {
	opts, err := ListResolutionStates(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(opts))
	for _, o := range opts {
		out[o.Value] = o.Label
	}
	return out, nil
}

// QueryAlertsWarehouse reads Alert.vAlert on the Data Warehouse instead of
// dbo.Alert on the Operational DB — SCOM prunes the Operational database's
// alert history on a short retention window, while the DW typically keeps it
// for years, so this is the only way to query older alerts. The DW's alert
// schema is missing a few fields the Operational one has, which this
// approximates as best it can:
//   - Owner isn't tracked in the DW at all; always empty here.
//   - There's no numeric-resolution-code-to-name reference table joinable in
//     the same query — the DW's dbo.ResolutionState keys off a GUID, not the
//     tinyint code Alert.vAlertResolutionState actually stores — so names are
//     resolved with a side lookup against the Operational connection instead.
//   - "Last modified" is approximated as the most recent resolution state
//     change (see buildAlertsWarehouseQuery), falling back to the raised time
//     for an alert that has never changed state.
func QueryAlertsWarehouse(ctx context.Context, warehouse *sql.DB, operational *sql.DB, f AlertFilter) (*data.Frame, error) {
	stateNames, err := resolutionStateNameMap(ctx, operational)
	if err != nil {
		return nil, fmt.Errorf("resolve resolution state names: %w", err)
	}

	query, args := buildAlertsWarehouseQuery(f)
	rows, err := warehouse.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var (
		alertIDs, names, entities, severities, priorities, resolutionStates, owners, descriptions []string
		repeatCounts                                                                              []int64
		timeRaised, lastModified                                                                  []time.Time
	)

	for rows.Next() {
		var alertID, name, entity, severity, priority, resolutionCode string
		var repeatCount int64
		var description sql.NullString
		var raised, modified time.Time

		if err := rows.Scan(&alertID, &name, &entity, &severity, &priority, &resolutionCode, &repeatCount, &raised, &modified, &description); err != nil {
			return nil, err
		}

		alertIDs = append(alertIDs, alertID)
		names = append(names, name)
		entities = append(entities, entity)
		severities = append(severities, severity)
		priorities = append(priorities, priority)
		resolutionState := stateNames[resolutionCode]
		if resolutionState == "" {
			resolutionState = resolutionCode
		}
		resolutionStates = append(resolutionStates, resolutionState)
		repeatCounts = append(repeatCounts, repeatCount)
		owners = append(owners, "") // Not tracked in the DW.
		timeRaised = append(timeRaised, raised)
		lastModified = append(lastModified, modified)
		descriptions = append(descriptions, description.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := data.NewFrame("alerts",
		data.NewField("time", nil, timeRaised),
		data.NewField("alertId", nil, alertIDs),
		data.NewField("name", nil, names),
		data.NewField("managedEntity", nil, entities),
		data.NewField("severity", nil, severities),
		data.NewField("priority", nil, priorities),
		data.NewField("resolutionState", nil, resolutionStates),
		data.NewField("repeatCount", nil, repeatCounts),
		data.NewField("owner", nil, owners),
		data.NewField("lastModified", nil, lastModified),
		data.NewField("description", nil, descriptions),
	)
	return frame, nil
}
