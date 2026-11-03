package scom

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// Health tree size limits. Grafana's Node Graph draws 200 nodes at a time but
// lays out every node it's sent, so a tree spanning a big group (every
// member, and everything their rollups reach) has to stop somewhere. The
// query keeps the top of the hierarchy and, level by level, the worst-health
// objects first; the nodes frame carries a notice whenever anything is cut.
const (
	// healthTreeMaxObjects caps how many objects one tree pulls in: the root,
	// a group's members and everything their rollup monitors reach.
	healthTreeMaxObjects = 500
	// healthTreeMaxDepth caps how many rollup hops the query follows. The
	// object cap already bounds the work; this only stops a pathological
	// chain early.
	healthTreeMaxDepth = 20
	// healthTreeMaxNodes caps how many monitors one tree returns.
	healthTreeMaxNodes = 2000
)

// healthTreeMembersTempTable holds a group tree's members. A group can have
// thousands, so they go through a temp table rather than an IN list — see
// idScopeTempTable.
const healthTreeMembersTempTable = "#HealthTreeMembers"

// healthTreeDisplayName is a monitor's display name, falling back to its
// internal name (NOT NULL) when it has no display string — what the SCOM
// console shows there too. See localizedNameApply.
const healthTreeDisplayName = "ISNULL(disp.LTValue, m.MonitorName)"

// healthTreeWorstFirst orders candidate objects (c, with b their
// dbo.BaseManagedEntity row and es their Entity Health dbo.State row) so the
// object cap keeps the unhealthy ones: Critical, Warning, Healthy, then
// unmonitored, then by name so the cut is the same on every refresh.
const healthTreeWorstFirst = `CASE es.HealthState WHEN 3 THEN 0 WHEN 2 THEN 1 WHEN 1 THEN 2 ELSE 3 END, b.DisplayName, c.Id`

// healthTreeAdmitSQL moves #HealthTreeCandidates into the tree and the next
// frontier, worst health first, as far as the object budget allows, and sets
// @Truncated when some don't fit.
var healthTreeAdmitSQL = fmt.Sprintf(`
	SET @Room = @MaxObjects - (SELECT COUNT(*) FROM #HealthTreeEntities);
	SELECT @Candidates = COUNT(*)
	FROM #HealthTreeCandidates c
	INNER JOIN dbo.BaseManagedEntity b ON b.BaseManagedEntityId = c.Id
	WHERE b.IsDeleted = 0;
	INSERT INTO #HealthTreeFrontier (Id)
	SELECT TOP (CASE WHEN @Room > 0 THEN @Room ELSE 0 END) c.Id
	FROM #HealthTreeCandidates c
	INNER JOIN dbo.BaseManagedEntity b ON b.BaseManagedEntityId = c.Id
	LEFT JOIN dbo.State es ON es.BaseManagedEntityId = c.Id
		AND es.MonitorId = @EntityHealthMonitorId
	WHERE b.IsDeleted = 0
	ORDER BY %s;
	IF @@ROWCOUNT < @Candidates SET @Truncated = 1;
	INSERT INTO #HealthTreeEntities (Id)
	SELECT f.Id FROM #HealthTreeFrontier f
	WHERE NOT EXISTS (SELECT 1 FROM #HealthTreeEntities e WHERE e.Id = f.Id);
	TRUNCATE TABLE #HealthTreeCandidates;
`, healthTreeWorstFirst)

// healthTreeExpansionSQL collects the objects in the tree (#HealthTreeEntities)
// and the rollup links between them (#HealthTreeLinks), the way Health
// Explorer walks SCOM's health model: a dependency (rollup) monitor on an
// object rolls up its member monitor from every object related to it, as the
// relationship's source, through the monitor's relationship type or any type
// derived from it. dbo.StateView exposes the same IsDependencyMonitor,
// RelationshipTypeId and MemberMonitorId columns for exactly this.
//
// It expects @rootId and #HealthTreeMembers (a group's members, empty for a
// single object). Members are part of the tree whether or not a rollup
// reaches them, so a group with no rollup monitors still shows what it
// contains (see newHealthTree).
//
// Iterative frontier expansion in temp tables, not recursive CTEs — the same
// reason as hostingExpansionSQL.
var healthTreeExpansionSQL = fmt.Sprintf(`
DECLARE @EntityHealthMonitorId uniqueidentifier = dbo.fn_ManagedTypeId_SystemHealthEntityState();
DECLARE @MaxObjects int = %[1]d;
DECLARE @MaxDepth int = %[2]d;
DECLARE @Depth int = 0;
DECLARE @Room int;
DECLARE @Candidates int;
DECLARE @Truncated bit = 0;

CREATE TABLE #HealthTreeRelTypes (
	BaseTypeId uniqueidentifier NOT NULL,
	TypeId uniqueidentifier NOT NULL,
	PRIMARY KEY (BaseTypeId, TypeId)
);
INSERT INTO #HealthTreeRelTypes (BaseTypeId, TypeId)
SELECT RelationshipTypeId, RelationshipTypeId
FROM dbo.RelationshipType;

WHILE @Depth < @MaxDepth
BEGIN
	INSERT INTO #HealthTreeRelTypes (BaseTypeId, TypeId)
	SELECT DISTINCT t.BaseTypeId, rt.RelationshipTypeId
	FROM #HealthTreeRelTypes t
	INNER JOIN dbo.RelationshipType rt ON rt.BaseRelationshipTypeId = t.TypeId
	WHERE NOT EXISTS (
		SELECT 1 FROM #HealthTreeRelTypes x
		WHERE x.BaseTypeId = t.BaseTypeId AND x.TypeId = rt.RelationshipTypeId);
	IF @@ROWCOUNT = 0 BREAK;
	SET @Depth = @Depth + 1;
END;

CREATE TABLE #HealthTreeEntities (Id uniqueidentifier PRIMARY KEY);
CREATE TABLE #HealthTreeFrontier (Id uniqueidentifier PRIMARY KEY);
CREATE TABLE #HealthTreeCandidates (Id uniqueidentifier PRIMARY KEY);
CREATE TABLE #HealthTreeLinks (
	SourceEntityId uniqueidentifier NOT NULL,
	MonitorId uniqueidentifier NOT NULL,
	TargetEntityId uniqueidentifier NOT NULL,
	MemberMonitorId uniqueidentifier NOT NULL,
	PRIMARY KEY (SourceEntityId, MonitorId, TargetEntityId)
);

INSERT INTO #HealthTreeEntities (Id)
SELECT b.BaseManagedEntityId
FROM dbo.BaseManagedEntity b
WHERE b.BaseManagedEntityId = @rootId AND b.IsDeleted = 0;

INSERT INTO #HealthTreeFrontier (Id)
SELECT Id FROM #HealthTreeEntities;

INSERT INTO #HealthTreeCandidates (Id)
SELECT mem.Id
FROM %[3]s mem
WHERE NOT EXISTS (SELECT 1 FROM #HealthTreeEntities e WHERE e.Id = mem.Id);
%[4]s
SET @Depth = 0;
WHILE @Depth < @MaxDepth AND EXISTS (SELECT 1 FROM #HealthTreeFrontier)
BEGIN
	INSERT INTO #HealthTreeLinks (SourceEntityId, MonitorId, TargetEntityId, MemberMonitorId)
	SELECT DISTINCT f.Id, dm.MonitorId, r.TargetEntityId, dm.MemberMonitorId
	FROM #HealthTreeFrontier f
	INNER JOIN dbo.State s ON s.BaseManagedEntityId = f.Id
	INNER JOIN dbo.Monitor dm ON dm.MonitorId = s.MonitorId
		AND dm.IsDependencyMonitor = 1
	INNER JOIN #HealthTreeRelTypes rt ON rt.BaseTypeId = dm.RelationshipTypeId
	INNER JOIN dbo.Relationship r ON r.SourceEntityId = f.Id
		AND r.RelationshipTypeId = rt.TypeId
		AND r.IsDeleted = 0
	INNER JOIN dbo.BaseManagedEntity tb ON tb.BaseManagedEntityId = r.TargetEntityId
		AND tb.IsDeleted = 0
	INNER JOIN dbo.State ts ON ts.BaseManagedEntityId = r.TargetEntityId
		AND ts.MonitorId = dm.MemberMonitorId;

	INSERT INTO #HealthTreeCandidates (Id)
	SELECT DISTINCT l.TargetEntityId
	FROM #HealthTreeLinks l
	INNER JOIN #HealthTreeFrontier f ON f.Id = l.SourceEntityId
	WHERE NOT EXISTS (SELECT 1 FROM #HealthTreeEntities e WHERE e.Id = l.TargetEntityId);

	TRUNCATE TABLE #HealthTreeFrontier;
%[4]s
	SET @Depth = @Depth + 1;
END;
IF EXISTS (SELECT 1 FROM #HealthTreeFrontier) SET @Truncated = 1;
`, healthTreeMaxObjects, healthTreeMaxDepth, healthTreeMembersTempTable, healthTreeAdmitSQL)

// healthTreeSelectSQL returns buildHealthTreeQuery's three result sets: every
// monitor's state on every object in the tree, the rollup links between them,
// and whether the object cap cut the tree short.
//
// Goes to dbo.Monitor rather than SCOM's dbo.MonitorView wrapper. The view
// adds exactly two things over the base table: a ManagementPack
// ContentReadable filter, replicated below verbatim, and a LocalizedText join
// that emits one row per installed language pack. See localizedNameApply,
// which resolves a name by preference instead, and
// monitorOperationalStateApply for the other fan-out this query has to
// collapse: both would otherwise multiply a dbo.State row into several, and
// duplicated rows become duplicate (object, monitor) node ids.
var healthTreeSelectSQL = fmt.Sprintf(`
SELECT
	CONVERT(varchar(64), s.BaseManagedEntityId) AS EntityId,
	CONVERT(varchar(64), m.MonitorId) AS MonitorId,
	CONVERT(varchar(64), m.ParentMonitorId) AS ParentMonitorId,
	%[1]s AS DisplayName,
	m.MonitorCategory AS Category,
	s.HealthState,
	%[2]s AS HealthStateName,
	s.LastModified,
	ISNULL(mm.IsInMaintenanceMode, 0) AS InMaintenanceMode,
	bme.DisplayName AS EntityDisplayName
FROM #HealthTreeEntities te
INNER JOIN dbo.State s ON s.BaseManagedEntityId = te.Id
INNER JOIN dbo.Monitor m ON m.MonitorId = s.MonitorId
INNER JOIN dbo.ManagementPack mp ON mp.ManagementPackId = m.ManagementPackId
	AND mp.ContentReadable = 1
INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId%[3]s%[4]s
LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId;

SELECT
	CONVERT(varchar(64), l.SourceEntityId) AS SourceEntityId,
	CONVERT(varchar(64), l.MonitorId) AS MonitorId,
	CONVERT(varchar(64), l.TargetEntityId) AS TargetEntityId,
	CONVERT(varchar(64), l.MemberMonitorId) AS MemberMonitorId
FROM #HealthTreeLinks l
INNER JOIN #HealthTreeEntities e ON e.Id = l.TargetEntityId;

SELECT @Truncated AS Truncated;`,
	healthTreeDisplayName,
	healthStateCase("mos", "s.HealthState"),
	localizedNameApply("m.MonitorId", "m.MonitorName", "disp"),
	monitorOperationalStateApply("mos", "s.MonitorId", "s.HealthState"))

// buildHealthTreeQuery builds the query+args for QueryHealthTree. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see healthtree_test.go.
//
// rootID is the object at the top of the tree. memberIDs is a group's members
// when the tree is rooted at a group, and empty otherwise.
//
// Deliberately has no fn_ManagedTypeId_SystemHealthEntityState() filter on
// the monitors, unlike buildHealthCurrentQuery — a health tree wants every
// monitor that applies to each object, not just its top-level rollup.
// Starting from dbo.State scoped to a BaseManagedEntityId already returns
// exactly the monitors that target that object's class, so there's no need
// to walk dbo.DerivedManagedTypes.
func buildHealthTreeQuery(rootID string, memberIDs []string) (string, []any) {
	membersSQL, args := idScopeTempTable(healthTreeMembersTempTable, memberIDs)
	if membersSQL == "" {
		membersSQL = "\nCREATE TABLE " + healthTreeMembersTempTable + " (Id uniqueidentifier PRIMARY KEY);\n"
	}
	args = append([]any{sql.Named("rootId", rootID)}, args...)
	return membersSQL + healthTreeExpansionSQL + healthTreeSelectSQL, args
}

// healthTreeMonitorRow is one row of buildHealthTreeQuery's first result set:
// one monitor's current state on one object.
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

// healthTreeLink is one row of buildHealthTreeQuery's second result set: the
// rollup (dependency) monitor MonitorID on SourceEntityID rolls up
// MemberMonitorID on TargetEntityID.
type healthTreeLink struct {
	SourceEntityID  string
	MonitorID       string
	TargetEntityID  string
	MemberMonitorID string
}

// healthTreeNodeID is a row's Node Graph node id. A monitor guid alone can't
// be one: the objects a tree spans are largely targeted by the *same*
// monitors (every Windows computer carries the identical "Logical Disk Free
// Space" monitor id). Keyed on the monitor alone, every object's copy of a
// monitor would collapse into one node.
func healthTreeNodeID(entityID, monitorID string) string {
	return entityID + ":" + monitorID
}

// healthTree is the monitor graph behind one health tree, shaped the way
// SCOM's Health Explorer shapes it:
//
//   - within an object, each monitor sits under its parent monitor;
//   - a rollup (dependency) monitor has the member monitor of every object it
//     rolls up beneath it: a group's availability rollup has each member's
//     Availability, a computer's operating system rollup has its OS's, and so
//     on down;
//   - group members that no rollup reaches (a group with no rollup monitors,
//     or members related to it some other way) sit directly under the root,
//     so the tree still shows what the group contains.
//
// Only nodes reachable from the root's top monitor (Entity Health) are in the
// tree, as in Health Explorer: an object whose Availability alone rolls up
// shows that subtree and not its Performance one.
type healthTree struct {
	rows     []healthTreeMonitorRow
	index    map[string]int // node id -> index into rows
	children map[string][]string
	parents  map[string][]string
	edges    map[[2]string]bool
	root     string
	// entries are the nodes where the tree enters an object: the root, each
	// object a rollup reaches and each member hung under the root. They're
	// titled with the object's name rather than the monitor's.
	entries map[string]bool
	// grafted are the members hung under the root because no rollup reaches
	// them.
	grafted map[string]bool
	reach   map[string]bool
}

// newHealthTree links up the query's rows. rootID is the tree's root object
// and memberIDs a group root's members (see buildHealthTreeQuery).
func newHealthTree(rows []healthTreeMonitorRow, links []healthTreeLink, rootID string, memberIDs []string) *healthTree {
	t := &healthTree{
		rows:     rows,
		index:    make(map[string]int, len(rows)),
		children: map[string][]string{},
		parents:  map[string][]string{},
		edges:    map[[2]string]bool{},
		entries:  map[string]bool{},
		grafted:  map[string]bool{},
		reach:    map[string]bool{},
	}

	// An object's top monitor (Entity Health) is its only one with no parent.
	tops := map[string]string{}
	for i, r := range rows {
		id := healthTreeNodeID(r.EntityID, r.MonitorID)
		t.index[id] = i
		if !r.ParentMonitorID.Valid {
			tops[r.EntityID] = id
		}
	}
	t.root = tops[rootID]
	if t.root == "" {
		return t
	}
	t.entries[t.root] = true

	for _, r := range rows {
		if !r.ParentMonitorID.Valid {
			continue
		}
		parent := healthTreeNodeID(r.EntityID, r.ParentMonitorID.String)
		if !t.has(parent) {
			// The parent has no state row here (its management pack isn't
			// readable, or SCOM hasn't calculated it yet), so hang the monitor
			// under its object's top monitor rather than lose it.
			parent = tops[r.EntityID]
		}
		t.addEdge(parent, healthTreeNodeID(r.EntityID, r.MonitorID))
	}

	linked := map[string]bool{}
	for _, l := range links {
		from := healthTreeNodeID(l.SourceEntityID, l.MonitorID)
		to := healthTreeNodeID(l.TargetEntityID, l.MemberMonitorID)
		if !t.has(from) || !t.has(to) {
			continue
		}
		t.addEdge(from, to)
		t.entries[to] = true
		linked[l.TargetEntityID] = true
	}

	reached := map[string]bool{} // objects with at least one node in the tree
	t.markReachable(t.root, reached)

	// Members that no other object rolls up go first, so a member that's
	// also, say, another member's operating system ends up under that member
	// rather than in two places.
	members := slices.Clone(memberIDs)
	slices.Sort(members)
	members = slices.Compact(members)
	for _, skipLinked := range []bool{true, false} {
		for _, m := range members {
			top := tops[m]
			if top == "" || top == t.root || reached[m] || (skipLinked && linked[m]) {
				continue
			}
			t.addEdge(t.root, top)
			t.entries[top] = true
			t.grafted[top] = true
			t.markReachable(top, reached)
		}
	}

	for id := range t.children {
		t.sortChildren(id)
	}
	return t
}

func (t *healthTree) has(id string) bool {
	_, ok := t.index[id]
	return ok
}

func (t *healthTree) row(id string) healthTreeMonitorRow {
	return t.rows[t.index[id]]
}

func (t *healthTree) addEdge(from, to string) {
	key := [2]string{from, to}
	if from == "" || from == to || t.edges[key] {
		return
	}
	t.edges[key] = true
	t.children[from] = append(t.children[from], to)
	t.parents[to] = append(t.parents[to], from)
}

// markReachable adds start and everything beneath it to the tree, recording
// the objects it touches in reached.
func (t *healthTree) markReachable(start string, reached map[string]bool) {
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t.reach[id] {
			continue
		}
		t.reach[id] = true
		reached[t.row(id).EntityID] = true
		stack = append(stack, t.children[id]...)
	}
}

// sortChildren orders a node's children so the frames come out the same on
// every refresh: the node's own object's monitors first, then other objects',
// each by name.
func (t *healthTree) sortChildren(parent string) {
	entity := t.row(parent).EntityID
	slices.SortFunc(t.children[parent], func(a, b string) int {
		ra, rb := t.row(a), t.row(b)
		return cmp.Or(
			boolCompare(ra.EntityID != entity, rb.EntityID != entity),
			strings.Compare(ra.EntityDisplayName, rb.EntityDisplayName),
			strings.Compare(ra.DisplayName, rb.DisplayName),
			strings.Compare(a, b),
		)
	})
}

func boolCompare(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// keep picks the nodes to show: the ones that match (Warning or Critical with
// unhealthyOnly, anything but Not Monitored without), every node on the way
// back up to the root from them, and the root itself — matching Health
// Explorer's "unhealthy only" view, and keeping a group tree in one piece
// even when the group's own health is Not Monitored. A Not Monitored monitor
// is noise unless something monitored sits beneath it (a disabled aggregate,
// say), in which case it stays as the connection.
func (t *healthTree) keep(unhealthyOnly bool) map[string]bool {
	keep := map[string]bool{}
	if t.root == "" {
		return keep
	}
	keep[t.root] = true

	var stack []string
	for id := range t.reach {
		state := t.row(id).HealthState
		if state == 2 || state == 3 || (!unhealthyOnly && state != 0) {
			stack = append(stack, id)
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if keep[id] {
			continue
		}
		keep[id] = true
		for _, p := range t.parents[id] {
			if t.reach[p] {
				stack = append(stack, p)
			}
		}
	}
	return keep
}

// order lists the kept nodes breadth first from the root, so the frame starts
// at the root and a tree cut at healthTreeMaxNodes keeps its upper levels.
func (t *healthTree) order(keep map[string]bool) []string {
	if !keep[t.root] {
		return nil
	}
	out := []string{t.root}
	seen := map[string]bool{t.root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range t.children[out[i]] {
			if keep[c] && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// healthTreeFrames builds the Node Graph "nodes"/"edges" frame pair from the
// query's rows and links. Split out from QueryHealthTree so it's directly
// testable without a live DB — see healthtree_test.go. truncated says the
// query already hit the object cap.
//
// Node Graph's field names are lowercase on the wire (verified against
// node_modules/@grafana/data/dist/types/utils/nodeGraph.d.ts in this repo,
// not just Grafana's docs): id, title, subtitle, mainstat, arc__<state>
// (float64 fields that must sum to ~1 per node, each colored via that field's
// own FieldConfig.Color), detail__<label>. Node Graph shows the title and
// subtitle under each node and the details in its context menu.
func healthTreeFrames(rows []healthTreeMonitorRow, links []healthTreeLink, rootID string, memberIDs []string, unhealthyOnly, truncated bool) []*data.Frame {
	t := newHealthTree(rows, links, rootID, memberIDs)

	order := t.order(t.keep(unhealthyOnly))
	if len(order) > healthTreeMaxNodes {
		order = order[:healthTreeMaxNodes]
		truncated = true
	}
	shown := make(map[string]bool, len(order))
	for _, id := range order {
		shown[id] = true
	}

	n := len(order)
	ids := make([]string, n)
	titles := make([]string, n)
	subtitles := make([]string, n)
	mainStats := make([]string, n)
	objects := make([]string, n)
	monitors := make([]string, n)
	categories := make([]string, n)
	lastModified := make([]string, n)
	maintenance := make([]string, n)
	arcHealthy := make([]float64, n)
	arcWarning := make([]float64, n)
	arcCritical := make([]float64, n)
	arcNotMonitored := make([]float64, n)
	grafted := 0

	for i, id := range order {
		r := t.row(id)
		ids[i] = id
		// Every node names both its monitor and its object. Where the tree
		// enters an object, the object is what you're looking for, so it
		// leads; everywhere else the monitor does.
		if t.entries[id] {
			titles[i], subtitles[i] = r.EntityDisplayName, r.DisplayName
		} else {
			titles[i], subtitles[i] = r.DisplayName, r.EntityDisplayName
		}
		mainStats[i] = r.HealthStateName
		objects[i] = r.EntityDisplayName
		monitors[i] = r.DisplayName
		categories[i] = r.Category.String
		lastModified[i] = r.LastModified.Format(time.RFC3339)
		maintenance[i] = "No"
		if r.InMaintenance {
			maintenance[i] = "Yes"
		}
		if t.grafted[id] {
			grafted++
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
		data.NewField("detail__Object", nil, objects),
		data.NewField("detail__Monitor", nil, monitors),
		data.NewField("detail__Category", nil, categories),
		data.NewField("detail__Last Modified", nil, lastModified),
		data.NewField("detail__Maintenance Mode", nil, maintenance),
		arcField("arc__healthy", arcHealthy, "green"),
		arcField("arc__warning", arcWarning, "orange"),
		arcField("arc__critical", arcCritical, "red"),
		arcField("arc__notmonitored", arcNotMonitored, "gray"),
	).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph})

	if truncated {
		text := "This health tree is too large to show in full, so part of it is left out."
		if !unhealthyOnly {
			text += " Turn on Unhealthy only to focus on the problems."
		}
		addNotice(nodesFrame, data.Notice{Severity: data.NoticeSeverityWarning, Text: text})
	}
	if grafted > 0 {
		text := fmt.Sprintf("%d of the group's members don't roll up into its health in SCOM, so they're shown directly under the group.", grafted)
		if grafted == 1 {
			text = "1 of the group's members doesn't roll up into its health in SCOM, so it's shown directly under the group."
		}
		addNotice(nodesFrame, data.Notice{Severity: data.NoticeSeverityInfo, Text: text})
	}

	// A node can sit under more than one parent (an object two rollups
	// reach), so an edge's id is its source and target together.
	var edgeIDs, sources, targets []string
	for _, id := range order {
		for _, c := range t.children[id] {
			if !shown[c] {
				continue
			}
			edgeIDs = append(edgeIDs, id+">"+c)
			sources = append(sources, id)
			targets = append(targets, c)
		}
	}

	edgesFrame := data.NewFrame("edges",
		data.NewField("id", nil, edgeIDs),
		data.NewField("source", nil, sources),
		data.NewField("target", nil, targets),
	).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeNodeGraph})

	return []*data.Frame{nodesFrame, edgesFrame}
}

// QueryHealthTree reads the Operational DB for a monitor hierarchy like SCOM's
// own Health Explorer — every monitor that applies, followed through rollup
// monitors into the objects they roll up — and renders it as a Grafana Node
// Graph node/edge frame pair.
//
// rootID is the object at the top of the tree. memberIDs is empty for a
// single-object tree; for a group it's the group's members, which sit under
// the group's rollup monitors where SCOM rolls them up and directly under the
// group otherwise (see newHealthTree).
//
// Not Monitored monitors are left out unless something monitored sits
// beneath them, and unhealthyOnly narrows the tree to Warning/Critical
// monitors and the path up to them — see healthTree.keep.
func QueryHealthTree(ctx context.Context, db *sql.DB, rootID string, memberIDs []string, unhealthyOnly bool) ([]*data.Frame, error) {
	query, args := buildHealthTreeQuery(rootID, memberIDs)
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
	if err := nextHealthTreeResultSet(rows); err != nil {
		return nil, err
	}

	var links []healthTreeLink
	for rows.Next() {
		var l healthTreeLink
		if err := rows.Scan(&l.SourceEntityID, &l.MonitorID, &l.TargetEntityID, &l.MemberMonitorID); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	if err := nextHealthTreeResultSet(rows); err != nil {
		return nil, err
	}

	var truncated bool
	for rows.Next() {
		if err := rows.Scan(&truncated); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return healthTreeFrames(monitorRows, links, rootID, memberIDs, unhealthyOnly, truncated), nil
}

// nextHealthTreeResultSet moves on to buildHealthTreeQuery's next result set.
func nextHealthTreeResultSet(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		return err
	}
	if !rows.NextResultSet() {
		if err := rows.Err(); err != nil {
			return err
		}
		return errors.New("the health tree query returned fewer result sets than expected")
	}
	return nil
}
