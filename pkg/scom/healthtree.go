package scom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// buildHealthTreeQuery builds the query+args for QueryHealthTree. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see healthtree_test.go.
//
// Deliberately has no fn_ManagedTypeId_SystemHealthEntityState() filter,
// unlike buildHealthCurrentQuery — a health tree wants every monitor that
// applies to the entity, not just its top-level rollup. Starting from
// dbo.State scoped to one BaseManagedEntityId already returns exactly the
// monitors that target this entity's class (SCOM's own class-targeting
// resolution happens for free), so there's no need to separately walk
// dbo.DerivedManagedTypes the way class/group scoping elsewhere does.
//
// mv.LanguageCode = 'ENU' is required for the same reason
// classesQueryByDisplayName (classes.go) pins it: dbo.MonitorView isn't
// pre-filtered to one language, so omitting this duplicates every row once
// per installed language pack.
func buildHealthTreeQuery(entityID string) (string, []any) {
	args := []any{sql.Named("entityId", entityID)}
	query := fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), mv.Id) AS MonitorId,
	CONVERT(varchar(64), mv.ParentMonitorId) AS ParentMonitorId,
	mv.DisplayName,
	mv.Category,
	s.HealthState,
	%s AS HealthStateName,
	s.LastModified,
	ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode,
	bme.DisplayName AS EntityDisplayName
FROM dbo.State s
INNER JOIN dbo.MonitorView mv ON mv.Id = s.MonitorId
INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId
LEFT JOIN dbo.MonitorOperationalState mos ON mos.MonitorId = s.MonitorId AND mos.HealthState = s.HealthState
LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId
WHERE s.BaseManagedEntityId = @entityId
	AND mv.LanguageCode = 'ENU'
ORDER BY mv.DisplayName`, healthStateCase("mos", "s.HealthState"))
	return query, args
}

// healthTreeMonitorRow is one scanned row from buildHealthTreeQuery.
type healthTreeMonitorRow struct {
	MonitorID         string
	ParentMonitorID   sql.NullString
	DisplayName       string
	Category          sql.NullString
	HealthState       int64
	HealthStateName   string
	LastModified      time.Time
	InMaintenance     bool
	EntityDisplayName string
}

// healthTreeFrames builds the Node Graph "nodes"/"edges" frame pair from
// scanned monitor rows for a single entity. Split out from QueryHealthTree
// so it's directly testable without a live DB — see healthtree_test.go.
//
// Node Graph's field names are lowercase on the wire (verified against
// node_modules/@grafana/data/dist/types/utils/nodeGraph.d.ts in this repo,
// not just Grafana's docs): id, title, subtitle, mainstat, arc__<state>
// (float64 fields that must sum to ~1 per node, each colored via that
// field's own FieldConfig.Color), detail__<label>.
func healthTreeFrames(rows []healthTreeMonitorRow) []*data.Frame {
	nodeIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		nodeIDs[r.MonitorID] = true
	}

	ids := make([]string, len(rows))
	titles := make([]string, len(rows))
	subtitles := make([]string, len(rows))
	mainStats := make([]string, len(rows))
	lastModifiedDetail := make([]string, len(rows))
	maintenanceDetail := make([]string, len(rows))
	arcHealthy := make([]float64, len(rows))
	arcWarning := make([]float64, len(rows))
	arcCritical := make([]float64, len(rows))
	arcNotMonitored := make([]float64, len(rows))

	for i, r := range rows {
		ids[i] = r.MonitorID
		// The root monitor (no parent) shows the entity's own name rather
		// than the literal "Entity Health" monitor name, matching Health
		// Explorer's actual look.
		title := r.DisplayName
		if !r.ParentMonitorID.Valid {
			title = r.EntityDisplayName
		}
		titles[i] = title
		subtitles[i] = r.Category.String
		mainStats[i] = r.HealthStateName
		lastModifiedDetail[i] = r.LastModified.Format(time.RFC3339)
		maintenanceDetail[i] = "No"
		if r.InMaintenance {
			maintenanceDetail[i] = "Yes"
		}

		// One-hot: exactly one arc field is 1 per row, matching this
		// monitor's current HealthState (0=Not Monitored/1=Healthy/
		// 2=Warning/3=Critical) — Node Graph requires arc values to sum to
		// ~1 per node.
		switch r.HealthState {
		case 1:
			arcHealthy[i] = 1
		case 2:
			arcWarning[i] = 1
		case 3:
			arcCritical[i] = 1
		default:
			arcNotMonitored[i] = 1
		}
	}

	arcField := func(name string, values []float64, color string) *data.Field {
		f := data.NewField(name, nil, values)
		f.Config = &data.FieldConfig{Color: map[string]interface{}{"mode": "fixed", "fixedColor": color}}
		return f
	}

	nodesFrame := data.NewFrame("nodes",
		data.NewField("id", nil, ids),
		data.NewField("title", nil, titles),
		data.NewField("subtitle", nil, subtitles),
		data.NewField("mainstat", nil, mainStats),
		data.NewField("detail__Last Modified", nil, lastModifiedDetail),
		data.NewField("detail__Maintenance Mode", nil, maintenanceDetail),
		arcField("arc__healthy", arcHealthy, "green"),
		arcField("arc__warning", arcWarning, "orange"),
		arcField("arc__critical", arcCritical, "red"),
		arcField("arc__notmonitored", arcNotMonitored, "gray"),
	).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph})

	// A row's parent is skipped — no edge emitted — when it's NULL (the
	// root) or when that parent monitor isn't itself one of this entity's
	// nodes (defensive: guards a parent monitor that, for whatever schema
	// edge case, has no dbo.State row for this entity).
	var edgeIDs, sources, targets []string
	for _, r := range rows {
		if !r.ParentMonitorID.Valid || !nodeIDs[r.ParentMonitorID.String] {
			continue
		}
		edgeIDs = append(edgeIDs, r.MonitorID)
		sources = append(sources, r.ParentMonitorID.String)
		targets = append(targets, r.MonitorID)
	}

	edgesFrame := data.NewFrame("edges",
		data.NewField("id", nil, edgeIDs),
		data.NewField("source", nil, sources),
		data.NewField("target", nil, targets),
	).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph})

	return []*data.Frame{nodesFrame, edgesFrame}
}

// QueryHealthTree reads dbo.State/dbo.MonitorView on the Operational DB for
// one managed entity's full monitor hierarchy — every monitor that applies
// to it, not just the top-level rollup QueryHealthCurrent shows — and
// renders it as a Grafana Node Graph node/edge frame pair matching what
// SCOM's own Health Explorer shows for that object.
func QueryHealthTree(ctx context.Context, db *sql.DB, entityID string) ([]*data.Frame, error) {
	query, args := buildHealthTreeQuery(entityID)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var monitorRows []healthTreeMonitorRow
	for rows.Next() {
		var r healthTreeMonitorRow
		var maint sql.NullBool

		if err := rows.Scan(&r.MonitorID, &r.ParentMonitorID, &r.DisplayName, &r.Category,
			&r.HealthState, &r.HealthStateName, &r.LastModified, &maint, &r.EntityDisplayName); err != nil {
			return nil, err
		}
		r.InMaintenance = maint.Bool
		monitorRows = append(monitorRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return healthTreeFrames(monitorRows), nil
}
