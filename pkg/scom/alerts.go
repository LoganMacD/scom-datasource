package scom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// AlertTimeFilter picks how an alerts query uses the time range. Unset
// (including on a query saved before this field existed) behaves like
// AlertTimeRaised, the only behaviour there used to be.
type AlertTimeFilter string

const (
	// AlertTimeRaised returns alerts raised inside the time range.
	AlertTimeRaised AlertTimeFilter = "raised"
	// AlertTimeActive returns alerts open at any point in the time range:
	// raised before it ends and not closed before it starts. An alert raised
	// yesterday that's still open shows up on a "last hour" dashboard.
	AlertTimeActive AlertTimeFilter = "active"
	// AlertTimeIgnore returns every alert matching the other filters,
	// whenever it was raised.
	AlertTimeIgnore AlertTimeFilter = "ignore"
)

func validAlertTimeFilter(f AlertTimeFilter) bool {
	switch f {
	case "", AlertTimeRaised, AlertTimeActive, AlertTimeIgnore:
		return true
	}
	return false
}

type AlertFilter struct {
	InstanceIDs     []string
	Severities      []int64 // 0=Information, 1=Warning, 2=Critical
	ResolutionState []int64 // 0=New, 255=Closed, others MP/user-defined
	TimeFilter      AlertTimeFilter
	From, To        time.Time
}

// resolutionStateClosed is SCOM's built-in Closed resolution state.
const resolutionStateClosed = 255

// ListResolutionStates reads dbo.ResolutionState, which holds both SCOM's
// four built-in states (New/Acknowledged/Resolved/Closed) and any custom ones
// an administrator has added (Operations Console > Administration > Alert
// Resolution States), so this can't be a fixed list on the frontend.
func ListResolutionStates(ctx context.Context, db *sql.DB) ([]Option, error) {
	return runOptionsQuery(ctx, db, `
SELECT CONVERT(varchar(3), ResolutionState), ResolutionStateName
FROM dbo.ResolutionState
ORDER BY ResolutionState`, nil)
}

// ResolutionStates backs the /resolution-states resource endpoint and the
// warehouse alert query's state names, cached for metadataCacheTTL.
func (db *DB) ResolutionStates(ctx context.Context) ([]Option, error) {
	return db.cache().options.get(ctx, "resolutionStates", metadataCacheTTL, func(ctx context.Context) ([]Option, error) {
		return ListResolutionStates(ctx, db.Operational)
	})
}

// alertScopeTempTable is the temp table buildAlertsQuery/
// buildAlertsWarehouseQuery populate with InstanceIDs. A hosting-expanded
// class/group selection can run into the thousands of ids, and folding that
// many into a literal IN list (one @param per id) both risks SQL Server's
// 2,100-parameters-per-query limit and can make the optimizer choke trying
// to fold it into the plan — see idScopeTempTable and the same fix in
// counters.go/performance.go.
const alertScopeTempTable = "#AlertScope"

// alertRowsTempTable holds the warehouse alerts in range, so their latest
// resolution state is looked up for those alerts only.
const alertRowsTempTable = "#AlertRows"

func alertSeverityCase(column string) string {
	return fmt.Sprintf("CASE %[1]s WHEN 0 THEN 'Information' WHEN 1 THEN 'Warning' WHEN 2 THEN 'Critical' ELSE CONVERT(varchar(3), %[1]s) END", column)
}

func alertPriorityCase(column string) string {
	return fmt.Sprintf("CASE %[1]s WHEN 0 THEN 'Low' WHEN 1 THEN 'Normal' WHEN 2 THEN 'High' ELSE CONVERT(varchar(3), %[1]s) END", column)
}

// alertFilterWhere builds the severity and resolution state filters shared
// by both alert sources. stateColumn is the resolution state expression to
// filter on.
func alertFilterWhere(f AlertFilter, stateColumn string) (severityWhere, stateWhere string, severityArgs, stateArgs []any) {
	if len(f.Severities) > 0 {
		strs := make([]string, len(f.Severities))
		for i, s := range f.Severities {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("sev", strs)
		severityWhere = " AND a.Severity IN " + inSQL
		severityArgs = inArgs
	}
	if len(f.ResolutionState) > 0 {
		strs := make([]string, len(f.ResolutionState))
		for i, s := range f.ResolutionState {
			strs[i] = fmt.Sprintf("%d", s)
		}
		inSQL, inArgs := inClause("res", strs)
		stateWhere = fmt.Sprintf(" AND %s IN %s", stateColumn, inSQL)
		stateArgs = inArgs
	}
	return severityWhere, stateWhere, severityArgs, stateArgs
}

// buildAlertsQuery builds the query+args for QueryAlerts. Kept separate from
// execution so the generated SQL/WHERE clause can be asserted against in
// tests without a live DB — see alerts_test.go.
//
// dbo.Alert.TimeRaised is nullable; TimeAdded (when SCOM stored the alert,
// never NULL) stands in for it. With the default time filter a NULL
// TimeRaised never matches, so that only matters for the other filters.
func buildAlertsQuery(f AlertFilter) (string, []any) {
	var args []any
	setupSQL := ""
	where := "1 = 1"

	if len(f.InstanceIDs) > 0 {
		instSetupSQL, instArgs := idScopeTempTable(alertScopeTempTable, f.InstanceIDs)
		setupSQL = instSetupSQL
		where += fmt.Sprintf(" AND a.BaseManagedEntityId IN (SELECT Id FROM %s)", alertScopeTempTable)
		args = append(args, instArgs...)
	}
	severityWhere, stateWhere, severityArgs, stateArgs := alertFilterWhere(f, "a.ResolutionState")
	where += severityWhere + stateWhere
	args = append(args, severityArgs...)
	args = append(args, stateArgs...)

	switch f.TimeFilter {
	case AlertTimeActive:
		// Open at some point in the range: raised before it ended, and either
		// still open or closed after it started.
		where += fmt.Sprintf(" AND ISNULL(a.TimeRaised, a.TimeAdded) <= @to AND (a.ResolutionState <> %d OR a.TimeResolved >= @from)", resolutionStateClosed)
		args = append(args, sql.Named("from", f.From), sql.Named("to", f.To))
	case AlertTimeIgnore:
		// No time condition.
	default:
		where += " AND a.TimeRaised >= @from AND a.TimeRaised <= @to"
		args = append(args, sql.Named("from", f.From), sql.Named("to", f.To))
	}

	query := setupSQL + fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), a.AlertId) AS AlertId,
	a.AlertName,
	bme.DisplayName AS ManagedEntity,
	%s AS Severity,
	%s AS Priority,
	ISNULL(rs.ResolutionStateName, CONVERT(varchar(3), a.ResolutionState)) AS ResolutionState,
	a.RepeatCount,
	a.Owner,
	ISNULL(a.TimeRaised, a.TimeAdded) AS TimeRaised,
	a.LastModified,
	a.AlertDescription
FROM dbo.Alert a
INNER JOIN dbo.BaseManagedEntity bme ON a.BaseManagedEntityId = bme.BaseManagedEntityId
LEFT JOIN dbo.ResolutionState rs ON rs.ResolutionState = a.ResolutionState
WHERE %s
ORDER BY ISNULL(a.TimeRaised, a.TimeAdded) DESC`, alertSeverityCase("a.Severity"), alertPriorityCase("a.Priority"), where)

	return query, args
}

// alertRows collects an alert query's columns, in the alerts frame's order.
type alertRows struct {
	ids, names, entities, severities, priorities, states, owners, descriptions []string
	repeatCounts                                                               []int64
	raised, modified                                                           []time.Time
}

func (r *alertRows) add(id, name, entity, severity, priority, state, owner, description string, repeatCount int64, raised, modified time.Time) {
	r.ids = append(r.ids, id)
	r.names = append(r.names, name)
	r.entities = append(r.entities, entity)
	r.severities = append(r.severities, severity)
	r.priorities = append(r.priorities, priority)
	r.states = append(r.states, state)
	r.owners = append(r.owners, owner)
	r.descriptions = append(r.descriptions, description)
	r.repeatCounts = append(r.repeatCounts, repeatCount)
	r.raised = append(r.raised, raised)
	r.modified = append(r.modified, modified)
}

// frame builds the "alerts" frame. stateColors adds severity and priority
// colors (see colorStateField).
func (r *alertRows) frame(stateColors bool) *data.Frame {
	severity := data.NewField("severity", nil, r.severities)
	priority := data.NewField("priority", nil, r.priorities)
	if stateColors {
		colorStateField(severity, severityColors.mappings())
		colorStateField(priority, priorityColors.mappings())
	}
	return data.NewFrame("alerts",
		data.NewField("time", nil, r.raised),
		data.NewField("alertId", nil, r.ids),
		data.NewField("name", nil, r.names),
		data.NewField("managedEntity", nil, r.entities),
		severity,
		priority,
		data.NewField("resolutionState", nil, r.states),
		data.NewField("repeatCount", nil, r.repeatCounts),
		data.NewField("owner", nil, r.owners),
		data.NewField("lastModified", nil, r.modified),
		data.NewField("description", nil, r.descriptions),
	)
}

// QueryAlerts reads dbo.Alert on the Operational DB, filtered to alerts for
// the selected instances, using the time range as f.TimeFilter says.
func QueryAlerts(ctx context.Context, db *sql.DB, f AlertFilter, stateColors bool) (*data.Frame, error) {
	query, args := buildAlertsQuery(f)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out alertRows
	for rows.Next() {
		var alertID, name, entity, severity, priority, resolutionState string
		var repeatCount int64
		var owner, description sql.NullString
		var raised, modified time.Time

		if err := rows.Scan(&alertID, &name, &entity, &severity, &priority, &resolutionState, &repeatCount, &owner, &raised, &modified, &description); err != nil {
			return nil, err
		}
		out.add(alertID, name, entity, severity, priority, resolutionState, owner.String, description.String, repeatCount, raised, modified)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out.frame(stateColors), nil
}

// buildAlertsWarehouseQuery builds the query+args for QueryAlertsWarehouse.
// Kept separate from execution so the generated SQL/WHERE clause can be
// asserted against in tests without a live DB — see alerts_test.go.
//
// Alert.vAlert has no "current resolution state" column — only
// Alert.vAlertResolutionState, a history of every state change — so the
// LatestState CTE picks the most recent row per alert via ROW_NUMBER. The
// alerts in range go into a temp table first, and the CTE only reads their
// history: computed over the whole table, ROW_NUMBER ran across every state
// change the warehouse has ever kept. A brand new alert that hasn't had a
// state row written yet falls back to resolution state 0 (New) via ISNULL,
// matching dbo.Alert's own default.
//
// The warehouse only supports alerts raised in the range (see
// QueryAlertsWarehouse), so f.TimeFilter isn't used here.
func buildAlertsWarehouseQuery(f AlertFilter) (string, []any) {
	var args []any
	setupSQL := ""
	where := "1 = 1"

	if len(f.InstanceIDs) > 0 {
		instSetupSQL, instArgs := idScopeTempTable(alertScopeTempTable, f.InstanceIDs)
		setupSQL = instSetupSQL
		where += fmt.Sprintf(" AND me.ManagedEntityGuid IN (SELECT Id FROM %s)", alertScopeTempTable)
		args = append(args, instArgs...)
	}
	severityWhere, stateWhere, severityArgs, stateArgs := alertFilterWhere(f, "ISNULL(ls.ResolutionState, 0)")
	where += severityWhere
	args = append(args, severityArgs...)
	args = append(args, stateArgs...)

	where += " AND a.RaisedDateTime >= @from AND a.RaisedDateTime <= @to"
	args = append(args, sql.Named("from", f.From), sql.Named("to", f.To))

	query := setupSQL + fmt.Sprintf(`
SELECT
	a.AlertGuid,
	a.AlertName,
	COALESCE(me.ManagedEntityDefaultName, me.Name, CONVERT(nvarchar(64), me.ManagedEntityGuid)) AS ManagedEntity,
	a.Severity,
	a.Priority,
	a.RepeatCount,
	a.RaisedDateTime,
	a.AlertDescription
INTO %[1]s
FROM Alert.vAlert a
INNER JOIN dbo.vManagedEntity me ON a.ManagedEntityRowId = me.ManagedEntityRowId
WHERE %[2]s;

;WITH LatestState AS (
	SELECT
		ars.AlertGuid,
		ars.ResolutionState,
		ars.StateSetDateTime,
		ROW_NUMBER() OVER (PARTITION BY ars.AlertGuid ORDER BY ars.StateSetDateTime DESC) AS rn
	FROM Alert.vAlertResolutionState ars
	WHERE ars.AlertGuid IN (SELECT AlertGuid FROM %[1]s)
)
SELECT
	CONVERT(varchar(64), a.AlertGuid) AS AlertId,
	a.AlertName,
	a.ManagedEntity,
	%[3]s AS Severity,
	%[4]s AS Priority,
	CONVERT(varchar(3), ISNULL(ls.ResolutionState, 0)) AS ResolutionState,
	a.RepeatCount,
	a.RaisedDateTime,
	ISNULL(ls.StateSetDateTime, a.RaisedDateTime) AS LastModified,
	a.AlertDescription
FROM %[1]s a
LEFT JOIN LatestState ls ON ls.AlertGuid = a.AlertGuid AND ls.rn = 1
WHERE 1 = 1%[5]s
ORDER BY a.RaisedDateTime DESC`, alertRowsTempTable, where, alertSeverityCase("a.Severity"), alertPriorityCase("a.Priority"), stateWhere)

	return query, args
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
//
// Only alerts raised in the range are supported: finding alerts that were
// open during the range means reading every alert's state history, which in
// a warehouse holding years of alerts is too expensive to run per panel. A
// query asking for another time filter gets a notice saying so.
func QueryAlertsWarehouse(ctx context.Context, db *DB, f AlertFilter, stateColors bool) (*data.Frame, error) {
	states, err := db.ResolutionStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve resolution state names: %w", err)
	}
	stateNames := make(map[string]string, len(states))
	for _, o := range states {
		stateNames[o.Value] = o.Label
	}

	query, args := buildAlertsWarehouseQuery(f)
	rows, err := db.Warehouse.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out alertRows
	for rows.Next() {
		var alertID, name, entity, severity, priority, resolutionCode string
		var repeatCount int64
		var description sql.NullString
		var raised, modified time.Time

		if err := rows.Scan(&alertID, &name, &entity, &severity, &priority, &resolutionCode, &repeatCount, &raised, &modified, &description); err != nil {
			return nil, err
		}
		resolutionState := stateNames[resolutionCode]
		if resolutionState == "" {
			resolutionState = resolutionCode
		}
		out.add(alertID, name, entity, severity, priority, resolutionState, "", description.String, repeatCount, raised, modified) // Owner isn't tracked in the DW.
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frame := out.frame(stateColors)
	if f.TimeFilter == AlertTimeActive || f.TimeFilter == AlertTimeIgnore {
		addNotice(frame, data.Notice{
			Severity: data.NoticeSeverityWarning,
			Text:     "The Data Warehouse source only shows alerts raised in the time range, so that's what this shows. Use the Operational source to see open alerts.",
		})
	}
	return frame, nil
}

// emptyAlertsFrame is an alerts frame with no rows, with a notice saying
// why: what a scope that matches nothing returns.
func emptyAlertsFrame(stateColors bool) *data.Frame {
	var empty alertRows
	frame := empty.frame(stateColors)
	addNotice(frame, emptyScopeNotice)
	return frame
}

func addNotice(frame *data.Frame, notice data.Notice) {
	if frame.Meta == nil {
		frame.Meta = &data.FrameMeta{}
	}
	frame.Meta.Notices = append(frame.Meta.Notices, notice)
}
