package scom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// healthTreeScopeTempTable is the temp table buildHealthTreeQuery populates
// with the entities whose monitors make up the tree. A single-entity tree
// needs no such indirection, but a group tree's entity list is every member
// of the group — easily into the hundreds — which is exactly the size that
// makes a literal IN list a problem. See idScopeTempTable, and the same fix
// in health.go/performance.go/alerts.go.
const healthTreeScopeTempTable = "#HealthTreeScope"

// buildHealthTreeQuery builds the query+args for QueryHealthTree. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see healthtree_test.go.
//
// entityIDs is the root entity alone for a single-instance tree, or the group
// plus every one of its members for a group tree — see QueryHealthTree. Rows
// carry their own BaseManagedEntityId because a monitor id is only unique
// *within* one entity: every computer in a group reports the very same
// System.Health.EntityState monitor guid, so the entity is half of a node's
// identity (see healthTreeNodeID).
//
// Deliberately has no fn_ManagedTypeId_SystemHealthEntityState() filter,
// unlike buildHealthCurrentQuery — a health tree wants every monitor that
// applies to the entity, not just its top-level rollup. Starting from
// dbo.State scoped to a BaseManagedEntityId already returns exactly the
// monitors that target that entity's class (SCOM's own class-targeting
// resolution happens for free), so there's no need to separately walk
// dbo.DerivedManagedTypes the way class/group scoping elsewhere does.
//
// Goes to dbo.Monitor rather than SCOM's dbo.MonitorView wrapper. The view
// adds exactly two things over the base table: a ManagementPack
// ContentReadable filter, replicated below verbatim, and a LocalizedText
// join that emits one row per installed language pack. Pinning that join to
// a single LanguageCode — which is what this query used to do — silently
// drops every monitor localized into some other language, plus every monitor
// with no display string at all (NULL LanguageCode matches nothing). See
// localizedNameApply, which resolves a name by preference instead, and
// monitorOperationalStateApply for the other fan-out this query has to
// collapse: both would otherwise multiply a dbo.State row into several, and
// duplicated rows here become duplicate (entity, monitor) node ids — exactly
// the collision healthTreeNodeID exists to prevent.
//
// m.MonitorName is the ISNULL fallback for a monitor with no display string,
// matching what the SCOM console falls back to. It's NOT NULL, which is what
// keeps DisplayName safe to scan into a plain string.
func buildHealthTreeQuery(entityIDs []string) (string, []any) {
	setupSQL, args := idScopeTempTable(healthTreeScopeTempTable, entityIDs)
	const displayName = "ISNULL(disp.LTValue, m.MonitorName)"
	query := setupSQL + fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), s.BaseManagedEntityId) AS EntityId,
	CONVERT(varchar(64), m.MonitorId) AS MonitorId,
	CONVERT(varchar(64), m.ParentMonitorId) AS ParentMonitorId,
	%s AS DisplayName,
	m.MonitorCategory AS Category,
	s.HealthState,
	%s AS HealthStateName,
	s.LastModified,
	ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode,
	bme.DisplayName AS EntityDisplayName
FROM dbo.State s
INNER JOIN dbo.Monitor m ON m.MonitorId = s.MonitorId
INNER JOIN dbo.ManagementPack mp ON mp.ManagementPackId = m.ManagementPackId
	AND mp.ContentReadable = 1
INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId%s%s
LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId
WHERE s.BaseManagedEntityId IN (SELECT Id FROM %s)
ORDER BY bme.DisplayName, %s`,
		displayName,
		healthStateCase("mos", "s.HealthState"),
		localizedNameApply("m.MonitorId", "disp"),
		monitorOperationalStateApply("s.MonitorId", "s.HealthState"),
		healthTreeScopeTempTable,
		displayName)
	return query, args
}

// healthTreeMonitorRow is one scanned row from buildHealthTreeQuery.
type healthTreeMonitorRow struct {
	EntityID          string
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

// healthTreeNodeID is a row's Node Graph node id. A monitor guid alone can't
// be one: dbo.State rows a group tree spans come from many entities, and the
// monitors targeting them are largely the *same* guids (every Windows
// computer carries the identical "Logical Disk Free Space" monitor id). Keyed
// on the monitor alone, every member's copy of a monitor would collapse into
// one node and the whole group would render as a single machine's tree.
func healthTreeNodeID(entityID, monitorID string) string {
	return entityID + ":" + monitorID
}

// healthTreeParents maps each row's node id to its parent's node id, and is
// the single place the tree's shape is decided — healthTreeFrames draws edges
// from it and filterUnhealthyBranches walks it upwards, so the two can't
// disagree about what the tree looks like.
//
// Within one entity a monitor's parent is mv.ParentMonitorId on that same
// entity. Across entities, a member's own root monitor (the one with no
// parent) is grafted onto rootID's root monitor: SCOM's real group rollup
// runs through dependency monitors whose membership configuration isn't
// readable from dbo.State, so the tree approximates it by hanging each member
// under the group's root rather than under the specific rollup monitor
// (Availability, Configuration, ...) that actually feeds from it.
//
// A parent that isn't itself among the rows is left unmapped rather than
// mapped to a dangling id — that's what lets filterMonitored drop a Not
// Monitored monitor from the middle of a tree without orphaning an edge
// pointing at a node that no longer exists.
func healthTreeParents(rows []healthTreeMonitorRow, rootID string) map[string]string {
	nodeIDs := make(map[string]bool, len(rows))
	var rootNodeID string
	for _, r := range rows {
		nodeIDs[healthTreeNodeID(r.EntityID, r.MonitorID)] = true
		if r.EntityID == rootID && !r.ParentMonitorID.Valid {
			rootNodeID = healthTreeNodeID(r.EntityID, r.MonitorID)
		}
	}

	parents := make(map[string]string, len(rows))
	for _, r := range rows {
		nodeID := healthTreeNodeID(r.EntityID, r.MonitorID)
		switch {
		case r.ParentMonitorID.Valid:
			parent := healthTreeNodeID(r.EntityID, r.ParentMonitorID.String)
			if nodeIDs[parent] {
				parents[nodeID] = parent
			}
		case r.EntityID != rootID && rootNodeID != "" && nodeID != rootNodeID:
			parents[nodeID] = rootNodeID
		}
	}
	return parents
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
func healthTreeFrames(rows []healthTreeMonitorRow, rootID string) []*data.Frame {
	parents := healthTreeParents(rows, rootID)

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
		ids[i] = healthTreeNodeID(r.EntityID, r.MonitorID)
		// An entity's root monitor (no parent) shows that entity's own name
		// rather than the literal "Entity Health" monitor name, matching
		// Health Explorer's actual look. In a group tree this is what labels
		// each member's subtree with its machine name.
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

	// A row with no entry in parents gets no edge: it's either the tree's own
	// root, or its parent monitor isn't among these rows at all (a Not
	// Monitored ancestor dropped by filterMonitored, or — defensively — a
	// parent that has no dbo.State row for this entity). See
	// healthTreeParents.
	var edgeIDs, sources, targets []string
	for _, r := range rows {
		nodeID := healthTreeNodeID(r.EntityID, r.MonitorID)
		parent, ok := parents[nodeID]
		if !ok {
			continue
		}
		edgeIDs = append(edgeIDs, nodeID)
		sources = append(sources, parent)
		targets = append(targets, nodeID)
	}

	edgesFrame := data.NewFrame("edges",
		data.NewField("id", nil, edgeIDs),
		data.NewField("source", nil, sources),
		data.NewField("target", nil, targets),
	).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph})

	return []*data.Frame{nodesFrame, edgesFrame}
}

// filterUnhealthyBranches prunes rows down to monitors that are themselves
// Warning(2)/Critical(3), plus every ancestor on the path back to the root —
// matching Health Explorer's own "show only unhealthy" filter. A row that is
// healthy/not-monitored is dropped unless some descendant of it survived the
// filter, so the tree structure above a problem is preserved while
// uninteresting sibling branches disappear.
//
// In a group tree the ancestor walk crosses the entity boundary, since
// healthTreeParents hangs each member's root monitor under the group's — so a
// single critical monitor on one member keeps that member's chain *and* the
// group root above it, while a member with nothing wrong disappears whole,
// root node included. That pruning is what makes a large group's tree
// legible at all: without it every member contributes its full monitor set.
func filterUnhealthyBranches(rows []healthTreeMonitorRow, rootID string) []healthTreeMonitorRow {
	parents := healthTreeParents(rows, rootID)

	keep := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.HealthState != 2 && r.HealthState != 3 {
			continue
		}
		for id := healthTreeNodeID(r.EntityID, r.MonitorID); !keep[id]; {
			keep[id] = true
			parent, ok := parents[id]
			if !ok {
				break
			}
			id = parent
		}
	}

	out := make([]healthTreeMonitorRow, 0, len(rows))
	for _, r := range rows {
		if keep[healthTreeNodeID(r.EntityID, r.MonitorID)] {
			out = append(out, r)
		}
	}
	return out
}

// filterMonitored unconditionally drops monitors in the "Not Monitored"
// health state (0) — these are monitors disabled for this instance/class
// and never contribute a real health signal, so they're just noise in the
// tree. Unlike filterUnhealthyBranches, this doesn't need to preserve
// ancestor structure: healthTreeFrames already skips an edge whose parent
// isn't present among the surviving nodes, so a dropped Not Monitored
// ancestor simply leaves its remaining descendants without an edge to it.
func filterMonitored(rows []healthTreeMonitorRow) []healthTreeMonitorRow {
	out := make([]healthTreeMonitorRow, 0, len(rows))
	for _, r := range rows {
		if r.HealthState != 0 {
			out = append(out, r)
		}
	}
	return out
}

// QueryHealthTree reads dbo.State/dbo.MonitorView on the Operational DB for
// a full monitor hierarchy — every monitor that applies, not just the
// top-level rollup QueryHealthCurrent shows — and renders it as a Grafana
// Node Graph node/edge frame pair matching what SCOM's own Health Explorer
// shows.
//
// rootID is the entity at the top of the tree. memberIDs is empty for a
// single-instance tree; for a group it's the group's members, whose own
// monitor trees are grafted under the group's root (see healthTreeParents).
// A group has to be expanded this way because a group entity carries nothing
// but the five standard System.Health rollup monitors — querying it alone
// yields the same three-node tree no matter what the group contains, since
// that count comes from SCOM's health model rather than from membership.
//
// Not Monitored monitors are always dropped (see filterMonitored). When
// unhealthyOnly is set, the result is further pruned to Warning/Critical
// monitors and their ancestor chain — see filterUnhealthyBranches, run first
// so its ancestor walk still sees any Not Monitored rows needed to reach the
// root.
func QueryHealthTree(ctx context.Context, db *sql.DB, rootID string, memberIDs []string, unhealthyOnly bool) ([]*data.Frame, error) {
	entityIDs := append([]string{rootID}, memberIDs...)
	query, args := buildHealthTreeQuery(entityIDs)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var monitorRows []healthTreeMonitorRow
	for rows.Next() {
		var r healthTreeMonitorRow
		var maint sql.NullBool

		if err := rows.Scan(&r.EntityID, &r.MonitorID, &r.ParentMonitorID, &r.DisplayName, &r.Category,
			&r.HealthState, &r.HealthStateName, &r.LastModified, &maint, &r.EntityDisplayName); err != nil {
			return nil, err
		}
		r.InMaintenance = maint.Bool
		monitorRows = append(monitorRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if unhealthyOnly {
		monitorRows = filterUnhealthyBranches(monitorRows, rootID)
	}
	monitorRows = filterMonitored(monitorRows)

	return healthTreeFrames(monitorRows, rootID), nil
}
