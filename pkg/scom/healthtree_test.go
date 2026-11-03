package scom

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

func TestBuildHealthTreeQuery(t *testing.T) {
	query, args := buildHealthTreeQuery("root-1", []string{"member-1", "member-2"})

	wantContain := []string{
		// Monitors come from the base table, with the one thing
		// dbo.MonitorView added that the base table doesn't carried over.
		"FROM #HealthTreeEntities te",
		"INNER JOIN dbo.State s ON s.BaseManagedEntityId = te.Id",
		"INNER JOIN dbo.Monitor m ON m.MonitorId = s.MonitorId",
		"INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId",
		"AND mp.ContentReadable = 1",
		// A monitor localized into some other language, or with no display
		// string at all, still has to reach the tree — see
		// localizedNameApply.
		"WHERE lt.LTStringId = m.MonitorId AND lt.LTStringType = 1",
		"CASE WHEN lt.LTValue = m.MonitorName THEN 1 ELSE 0 END",
		"ISNULL(disp.LTValue, m.MonitorName) AS DisplayName",
		// (MonitorId, HealthState) is not unique in
		// dbo.MonitorOperationalState, so a plain LEFT JOIN would fan out
		// and duplicate node ids.
		"SELECT TOP 1 mosi.MonitorOperationalStateName",
		"WHERE mosi.MonitorId = s.MonitorId AND mosi.HealthState = s.HealthState",
		"ISNULL(mos.MonitorOperationalStateName, CASE s.HealthState",
		"LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId",
		// A monitor guid repeats across objects, so the object has to come
		// back on every row to build a unique node id.
		"CONVERT(varchar(64), s.BaseManagedEntityId) AS EntityId",
		"bme.DisplayName AS EntityDisplayName",

		// The tree follows rollup (dependency) monitors into the objects
		// they roll up, the way Health Explorer does: through the monitor's
		// relationship type or any type derived from it, from the related
		// object's member monitor.
		"WHERE b.BaseManagedEntityId = @rootId AND b.IsDeleted = 0",
		"FROM #HealthTreeMembers mem",
		"INNER JOIN dbo.RelationshipType rt ON rt.BaseRelationshipTypeId = t.TypeId",
		"AND dm.IsDependencyMonitor = 1",
		"INNER JOIN #HealthTreeRelTypes rt ON rt.BaseTypeId = dm.RelationshipTypeId",
		"INNER JOIN dbo.Relationship r ON r.SourceEntityId = f.Id",
		"AND r.RelationshipTypeId = rt.TypeId",
		"AND r.IsDeleted = 0",
		"AND ts.MonitorId = dm.MemberMonitorId",
		// Bounded, worst health first, and says when it cut.
		fmt.Sprintf("DECLARE @MaxObjects int = %d;", healthTreeMaxObjects),
		fmt.Sprintf("DECLARE @MaxDepth int = %d;", healthTreeMaxDepth),
		"ORDER BY " + healthTreeWorstFirst,
		"IF @@ROWCOUNT < @Candidates SET @Truncated = 1;",
		// Three result sets: monitors, rollup links, truncation flag.
		"CONVERT(varchar(64), l.MemberMonitorId) AS MemberMonitorId",
		"FROM #HealthTreeLinks l",
		"SELECT @Truncated AS Truncated;",
	}
	for _, want := range wantContain {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
	if strings.Contains(query, "%!") {
		t.Errorf("query has a leftover Sprintf verb\nfull query:\n%s", query)
	}

	// A health tree wants every monitor on each object, not just the
	// top-level rollup buildHealthCurrentQuery filters to.
	if strings.Contains(query, "s.MonitorId = dbo.fn_ManagedTypeId_SystemHealthEntityState()") {
		t.Errorf("query should not filter to the single rollup monitor\nfull query:\n%s", query)
	}

	// Pinning one LanguageCode is what used to drop monitors localized into
	// any other language (this deployment runs ENU and ENA), and monitors
	// with no display string at all. Language is a preference now.
	if strings.Contains(query, "LanguageCode = '") {
		t.Errorf("query filters on a single LanguageCode, want a preference order\nfull query:\n%s", query)
	}
	if !strings.Contains(query, "CASE lt.LanguageCode WHEN 'ENU' THEN 0 WHEN 'ENA' THEN 1 ELSE 2 END") {
		t.Errorf("query missing the ENU/ENA language preference order\nfull query:\n%s", query)
	}

	if len(args) != 2 {
		t.Fatalf("got %d args, want 2 (root and members)", len(args))
	}
	if root, ok := args[0].(sql.NamedArg); !ok || root.Name != "rootId" || root.Value != "root-1" {
		t.Errorf("args[0] = %v, want @rootId = root-1", args[0])
	}

	// A group can have thousands of members, which is exactly the size that
	// has to go through the temp table rather than a literal IN list.
	t.Run("many members still bind a single parameter", func(t *testing.T) {
		ids := make([]string, 0, 3000)
		for i := 0; i < 3000; i++ {
			ids = append(ids, fmt.Sprintf("member-%d", i))
		}
		_, manyArgs := buildHealthTreeQuery("root-1", ids)
		if len(manyArgs) != 2 {
			t.Errorf("got %d args for 3000 members, want 2 (SQL Server caps an RPC at ~2,100)", len(manyArgs))
		}
	})

	t.Run("no members still creates the members table", func(t *testing.T) {
		single, singleArgs := buildHealthTreeQuery("root-1", nil)
		if !strings.Contains(single, "CREATE TABLE #HealthTreeMembers (Id uniqueidentifier PRIMARY KEY);") {
			t.Errorf("query reads #HealthTreeMembers without creating it\nfull query:\n%s", single)
		}
		if len(singleArgs) != 1 {
			t.Errorf("got %d args, want 1 (root only)", len(singleArgs))
		}
	})
}

// monitorRow builds a health tree row for tests. parent "" means the
// object's top monitor (Entity Health).
func monitorRow(entity, monitor, parent, name string, state int64, entityName string) healthTreeMonitorRow {
	r := healthTreeMonitorRow{
		EntityID:          entity,
		MonitorID:         monitor,
		DisplayName:       name,
		HealthState:       state,
		HealthStateName:   []string{"Not Monitored", "Healthy", "Warning", "Critical"}[state],
		LastModified:      time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		EntityDisplayName: entityName,
	}
	if parent != "" {
		r.ParentMonitorID = sql.NullString{String: parent, Valid: true}
	}
	return r
}

// treeView is the parts of a health tree's frames the tests look at.
type treeView struct {
	ids       []string
	titles    map[string]string
	subtitles map[string]string
	details   map[string]map[string]string // node id -> detail label -> value
	parents   map[string][]string          // node id -> parent node ids
	edgeIDs   []string
	notices   []data.Notice
}

func viewHealthTree(t *testing.T, frames []*data.Frame) treeView {
	t.Helper()
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (nodes, edges)", len(frames))
	}
	nodes, edges := frames[0], frames[1]
	if nodes.Name != "nodes" || edges.Name != "edges" {
		t.Fatalf("got frame names %q, %q, want \"nodes\", \"edges\"", nodes.Name, edges.Name)
	}
	for _, f := range frames {
		if f.Meta == nil || f.Meta.PreferredVisualization != data.VisTypeNodeGraph {
			t.Errorf("frame %q missing PreferredVisualization=nodeGraph: %+v", f.Name, f.Meta)
		}
	}

	v := treeView{
		titles:    map[string]string{},
		subtitles: map[string]string{},
		details:   map[string]map[string]string{},
		parents:   map[string][]string{},
		notices:   nodes.Meta.Notices,
	}
	field := func(f *data.Frame, name string) *data.Field {
		fld, _ := f.FieldByName(name)
		if fld == nil {
			t.Fatalf("frame %q has no %q field", f.Name, name)
		}
		return fld
	}
	idField := field(nodes, "id")
	for i := 0; i < idField.Len(); i++ {
		id := idField.At(i).(string)
		if _, dup := v.titles[id]; dup {
			t.Errorf("duplicate node id %q", id)
		}
		v.ids = append(v.ids, id)
		v.titles[id] = field(nodes, "title").At(i).(string)
		v.subtitles[id] = field(nodes, "subtitle").At(i).(string)
		v.details[id] = map[string]string{}
		for _, f := range nodes.Fields {
			if label, ok := strings.CutPrefix(f.Name, "detail__"); ok {
				v.details[id][label] = f.At(i).(string)
			}
		}
	}

	edgeIDField, sources, targets := field(edges, "id"), field(edges, "source"), field(edges, "target")
	for i := 0; i < edgeIDField.Len(); i++ {
		source, target := sources.At(i).(string), targets.At(i).(string)
		if _, ok := v.titles[source]; !ok {
			t.Errorf("edge %s -> %s starts at a node that isn't in the frame", source, target)
		}
		if _, ok := v.titles[target]; !ok {
			t.Errorf("edge %s -> %s ends at a node that isn't in the frame", source, target)
		}
		v.edgeIDs = append(v.edgeIDs, edgeIDField.At(i).(string))
		v.parents[target] = append(v.parents[target], source)
	}
	return v
}

// assertConnected checks every node but the first (the root) has a parent,
// since Node Graph treats a node without one as another root.
func (v treeView) assertConnected(t *testing.T) {
	t.Helper()
	for i, id := range v.ids {
		if i > 0 && len(v.parents[id]) == 0 {
			t.Errorf("node %q (%q) has no parent, so it floats loose from the root", id, v.titles[id])
		}
	}
}

func (v treeView) assertNodes(t *testing.T, want ...string) {
	t.Helper()
	got := slices.Sorted(slices.Values(v.ids))
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("nodes = %v, want %v", got, want)
	}
}

func (v treeView) assertParents(t *testing.T, node string, want ...string) {
	t.Helper()
	got := slices.Sorted(slices.Values(v.parents[node]))
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Errorf("parents of %s = %v, want %v", node, got, want)
	}
}

func (v treeView) assertLabel(t *testing.T, node, title, subtitle string) {
	t.Helper()
	if v.titles[node] != title || v.subtitles[node] != subtitle {
		t.Errorf("%s labelled %q / %q, want %q / %q", node, v.titles[node], v.subtitles[node], title, subtitle)
	}
}

func noticeTexts(notices []data.Notice) string {
	var texts []string
	for _, n := range notices {
		texts = append(texts, n.Text)
	}
	return strings.Join(texts, " | ")
}

// A computer's own monitors stop at rollups like "Operating System
// Availability Rollup"; the problems are on the objects those roll up. The
// tree has to follow the rollup into the operating system, as Health
// Explorer does, and label every node with its object as well as its monitor.
func TestHealthTreeFramesFollowsRollups(t *testing.T) {
	const computer, os = "server01.contoso.example.com", "Microsoft Windows Server 2019 Datacenter"
	rows := []healthTreeMonitorRow{
		monitorRow("c", "health", "", "Entity Health", 3, computer),
		monitorRow("c", "avail", "health", "Availability", 3, computer),
		monitorRow("c", "heartbeat", "avail", "Health Service Heartbeat Failure", 1, computer),
		monitorRow("c", "osRollup", "avail", "Operating System Availability Rollup", 3, computer),
		monitorRow("c", "perf", "health", "Performance", 0, computer),
		monitorRow("os", "health", "", "Entity Health", 3, os),
		monitorRow("os", "avail", "health", "Availability", 3, os),
		monitorRow("os", "disk", "avail", "Logical Disk Free Space", 3, os),
		monitorRow("os", "perf", "health", "Performance", 1, os),
	}
	links := []healthTreeLink{{SourceEntityID: "c", MonitorID: "osRollup", TargetEntityID: "os", MemberMonitorID: "avail"}}

	v := viewHealthTree(t, healthTreeFrames(rows, links, "c", nil, false, false))

	if v.ids[0] != "c:health" {
		t.Errorf("first node = %s, want the root c:health", v.ids[0])
	}
	// The OS only rolls its Availability up into the computer, so that's
	// the only part of it in the tree. The computer's Not Monitored
	// Performance has nothing monitored beneath it.
	v.assertNodes(t, "c:health", "c:avail", "c:heartbeat", "c:osRollup", "os:avail", "os:disk")
	v.assertConnected(t)
	v.assertParents(t, "os:avail", "c:osRollup")
	v.assertParents(t, "os:disk", "os:avail")

	t.Run("nodes show display names, with the object leading where the tree enters it", func(t *testing.T) {
		v.assertLabel(t, "c:health", computer, "Entity Health")
		v.assertLabel(t, "c:avail", "Availability", computer)
		v.assertLabel(t, "c:osRollup", "Operating System Availability Rollup", computer)
		v.assertLabel(t, "os:avail", os, "Availability")
		v.assertLabel(t, "os:disk", "Logical Disk Free Space", os)
	})

	t.Run("details name the object and the monitor", func(t *testing.T) {
		d := v.details["os:disk"]
		if d["Object"] != os || d["Monitor"] != "Logical Disk Free Space" {
			t.Errorf("os:disk details = %v", d)
		}
		for _, label := range []string{"Category", "Last Modified", "Maintenance Mode"} {
			if _, ok := d[label]; !ok {
				t.Errorf("os:disk details missing %q: %v", label, d)
			}
		}
	})

	if len(v.notices) != 0 {
		t.Errorf("got notices %q, want none", noticeTexts(v.notices))
	}
}

func TestHealthTreeFramesArcs(t *testing.T) {
	rows := []healthTreeMonitorRow{
		monitorRow("e1", "health", "", "Entity Health", 3, "server01"),
		monitorRow("e1", "healthy", "health", "A", 1, "server01"),
		monitorRow("e1", "warning", "health", "B", 2, "server01"),
		monitorRow("e1", "critical", "health", "C", 3, "server01"),
		monitorRow("e1", "connector", "health", "D", 0, "server01"),
		monitorRow("e1", "below", "connector", "E", 1, "server01"),
	}
	nodes := healthTreeFrames(rows, nil, "e1", nil, false, false)[0]
	ids, _ := nodes.FieldByName("id")
	arcs := map[string]int64{"arc__healthy": 1, "arc__warning": 2, "arc__critical": 3, "arc__notmonitored": 0}
	for i := 0; i < ids.Len(); i++ {
		state := rows[slices.IndexFunc(rows, func(r healthTreeMonitorRow) bool {
			return healthTreeNodeID(r.EntityID, r.MonitorID) == ids.At(i).(string)
		})].HealthState
		sum := 0.0
		for name, arcState := range arcs {
			f, _ := nodes.FieldByName(name)
			got := f.At(i).(float64)
			sum += got
			if want := map[bool]float64{true: 1}[arcState == state]; got != want {
				t.Errorf("%s[%s] = %v, want %v", name, ids.At(i), got, want)
			}
		}
		if sum != 1 {
			t.Errorf("arcs for %s sum to %v, want 1", ids.At(i), sum)
		}
	}
}

// groupRows is a group "g" of two computers, m1 (healthy) and m2 (critical),
// which carry the *same* monitor guids as every other Windows computer does.
func groupRows(groupState int64) []healthTreeMonitorRow {
	return []healthTreeMonitorRow{
		monitorRow("g", "health", "", "Entity Health", groupState, "All Servers"),
		monitorRow("g", "avail", "health", "Availability", groupState, "All Servers"),
		monitorRow("g", "availRollup", "avail", "Group Availability Rollup", groupState, "All Servers"),
		monitorRow("g", "perf", "health", "Performance", 0, "All Servers"),
		monitorRow("m1", "health", "", "Entity Health", 1, "server01"),
		monitorRow("m1", "avail", "health", "Availability", 1, "server01"),
		monitorRow("m1", "heartbeat", "avail", "Heartbeat", 1, "server01"),
		monitorRow("m1", "perf", "health", "Performance", 1, "server01"),
		monitorRow("m2", "health", "", "Entity Health", 3, "server02"),
		monitorRow("m2", "avail", "health", "Availability", 3, "server02"),
		monitorRow("m2", "heartbeat", "avail", "Heartbeat", 3, "server02"),
		monitorRow("m2", "perf", "health", "Performance", 1, "server02"),
	}
}

// A group that rolls its members' health up (a dependency monitor over its
// membership relationship) breaks down through that rollup, member by
// member, like Health Explorer — not with every member dangling off the
// group's root beside the rollup that actually carries their health.
func TestHealthTreeFramesGroupWithRollup(t *testing.T) {
	links := []healthTreeLink{
		{SourceEntityID: "g", MonitorID: "availRollup", TargetEntityID: "m1", MemberMonitorID: "avail"},
		{SourceEntityID: "g", MonitorID: "availRollup", TargetEntityID: "m2", MemberMonitorID: "avail"},
	}
	v := viewHealthTree(t, healthTreeFrames(groupRows(3), links, "g", []string{"m1", "m2"}, false, false))

	v.assertNodes(t,
		"g:health", "g:avail", "g:availRollup",
		"m1:avail", "m1:heartbeat",
		"m2:avail", "m2:heartbeat")
	v.assertConnected(t)
	v.assertParents(t, "m1:avail", "g:availRollup")
	v.assertParents(t, "m2:avail", "g:availRollup")
	v.assertParents(t, "m1:heartbeat", "m1:avail")

	v.assertLabel(t, "g:health", "All Servers", "Entity Health")
	v.assertLabel(t, "m1:avail", "server01", "Availability")
	v.assertLabel(t, "m2:heartbeat", "Heartbeat", "server02")

	if len(v.notices) != 0 {
		t.Errorf("got notices %q, want none: every member rolls up", noticeTexts(v.notices))
	}
}

// A group with no rollup monitors has no health of its own (Not Monitored),
// which used to drop its root and leave each member's tree floating loose.
// The root stays, members hang under it, and a notice says why.
func TestHealthTreeFramesGroupWithoutRollup(t *testing.T) {
	rows := groupRows(0)
	// m0 is m2's operating system, also a member of the group, and rolled up
	// by m2. It belongs under m2, not under the group a second time. Its id
	// sorts before m2's on purpose: members are visited in id order, and
	// that order mustn't decide where m0 ends up.
	rows = append(rows,
		monitorRow("m2", "osRollup", "avail", "Operating System Availability Rollup", 3, "server02"),
		monitorRow("m0", "health", "", "Entity Health", 3, "Windows Server 2019"),
		monitorRow("m0", "avail", "health", "Availability", 3, "Windows Server 2019"),
	)
	links := []healthTreeLink{{SourceEntityID: "m2", MonitorID: "osRollup", TargetEntityID: "m0", MemberMonitorID: "avail"}}

	v := viewHealthTree(t, healthTreeFrames(rows, links, "g", []string{"m2", "m1", "m0"}, false, false))

	if v.ids[0] != "g:health" {
		t.Fatalf("first node = %s, want the group root g:health", v.ids[0])
	}
	v.assertConnected(t)
	v.assertParents(t, "m1:health", "g:health")
	v.assertParents(t, "m2:health", "g:health")
	v.assertParents(t, "m0:avail", "m2:osRollup")
	if _, ok := v.titles["m0:health"]; ok {
		t.Error("m0 was hung under the group as well as under the member that rolls it up")
	}
	for _, id := range []string{"g:avail", "g:availRollup", "g:perf"} {
		if _, ok := v.titles[id]; ok {
			t.Errorf("%s is Not Monitored with nothing monitored beneath it, want it left out", id)
		}
	}

	v.assertLabel(t, "g:health", "All Servers", "Entity Health")
	v.assertLabel(t, "m1:health", "server01", "Entity Health")
	v.assertLabel(t, "m1:avail", "Availability", "server01")
	v.assertLabel(t, "m0:avail", "Windows Server 2019", "Availability")

	if got := noticeTexts(v.notices); !strings.Contains(got, "2 of the group's members don't roll up into its health") {
		t.Errorf("notices = %q, want one saying 2 members are shown directly under the group", got)
	}
}

// Node ids are (object, monitor): keyed on the monitor alone, every member's
// copy of a shared monitor collapsed into one node.
func TestHealthTreeFramesUniqueNodeIDs(t *testing.T) {
	v := viewHealthTree(t, healthTreeFrames(groupRows(1), nil, "g", []string{"m1", "m2"}, false, false))
	// viewHealthTree fails on a duplicate id; check both members' shared
	// monitors made it in separately.
	for _, id := range []string{"m1:avail", "m2:avail", "m1:heartbeat", "m2:heartbeat"} {
		if _, ok := v.titles[id]; !ok {
			t.Errorf("missing node %s", id)
		}
	}
}

func TestHealthTreeFramesUnhealthyOnly(t *testing.T) {
	links := []healthTreeLink{
		{SourceEntityID: "g", MonitorID: "availRollup", TargetEntityID: "m1", MemberMonitorID: "avail"},
		{SourceEntityID: "g", MonitorID: "availRollup", TargetEntityID: "m2", MemberMonitorID: "avail"},
	}

	t.Run("healthy members drop out whole, unhealthy ones keep the path to the root", func(t *testing.T) {
		v := viewHealthTree(t, healthTreeFrames(groupRows(3), links, "g", []string{"m1", "m2"}, true, false))
		v.assertNodes(t, "g:health", "g:avail", "g:availRollup", "m2:avail", "m2:heartbeat")
		v.assertConnected(t)
	})

	t.Run("a group without rollups keeps its root above unhealthy members", func(t *testing.T) {
		v := viewHealthTree(t, healthTreeFrames(groupRows(0), nil, "g", []string{"m1", "m2"}, true, false))
		v.assertNodes(t, "g:health", "m2:health", "m2:avail", "m2:heartbeat")
		v.assertConnected(t)
	})

	t.Run("an all-healthy tree comes down to its root", func(t *testing.T) {
		rows := []healthTreeMonitorRow{
			monitorRow("e1", "health", "", "Entity Health", 1, "server01"),
			monitorRow("e1", "avail", "health", "Availability", 1, "server01"),
		}
		v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, true, false))
		v.assertNodes(t, "e1:health")
	})
}

func TestHealthTreeFramesNotMonitored(t *testing.T) {
	rows := []healthTreeMonitorRow{
		monitorRow("e1", "health", "", "Entity Health", 3, "server01"),
		// A disabled aggregate (Not Monitored) with a critical monitor under
		// it stays, so the critical one isn't cut off from the root.
		monitorRow("e1", "disabled", "health", "Custom Aggregate", 0, "server01"),
		monitorRow("e1", "disk", "disabled", "Disk Free Space", 3, "server01"),
		// A Not Monitored leaf is just noise.
		monitorRow("e1", "unused", "health", "Unused Monitor", 0, "server01"),
	}
	v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, false, false))
	v.assertNodes(t, "e1:health", "e1:disabled", "e1:disk")
	v.assertConnected(t)

	t.Run("the root stays even when nothing is monitored", func(t *testing.T) {
		rows := []healthTreeMonitorRow{
			monitorRow("e1", "health", "", "Entity Health", 0, "server01"),
			monitorRow("e1", "avail", "health", "Availability", 0, "server01"),
		}
		v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, false, false))
		v.assertNodes(t, "e1:health")
	})
}

// A monitor whose parent has no state row (an unreadable management pack,
// or one SCOM hasn't calculated yet) hangs under its object's top monitor
// instead of floating loose or disappearing.
func TestHealthTreeFramesMissingParent(t *testing.T) {
	rows := []healthTreeMonitorRow{
		monitorRow("e1", "health", "", "Entity Health", 3, "server01"),
		monitorRow("e1", "orphan", "does-not-exist", "Orphaned Monitor", 3, "server01"),
	}
	v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, false, false))
	v.assertNodes(t, "e1:health", "e1:orphan")
	v.assertParents(t, "e1:orphan", "e1:health")
}

// An object two rollups reach sits under both, once, with distinct edge ids.
func TestHealthTreeFramesSharedChild(t *testing.T) {
	rows := []healthTreeMonitorRow{
		monitorRow("g", "health", "", "Entity Health", 2, "Web Farm"),
		monitorRow("g", "rollupA", "health", "Rollup A", 2, "Web Farm"),
		monitorRow("g", "rollupB", "health", "Rollup B", 2, "Web Farm"),
		monitorRow("m1", "health", "", "Entity Health", 2, "web01"),
	}
	links := []healthTreeLink{
		{SourceEntityID: "g", MonitorID: "rollupA", TargetEntityID: "m1", MemberMonitorID: "health"},
		{SourceEntityID: "g", MonitorID: "rollupB", TargetEntityID: "m1", MemberMonitorID: "health"},
	}
	v := viewHealthTree(t, healthTreeFrames(rows, links, "g", []string{"m1"}, false, false))
	v.assertParents(t, "m1:health", "g:rollupA", "g:rollupB")
	if len(slices.Compact(slices.Sorted(slices.Values(v.edgeIDs)))) != len(v.edgeIDs) {
		t.Errorf("edge ids aren't unique: %v", v.edgeIDs)
	}
}

// Rollups that reach back into each other mustn't hang the tree walk.
func TestHealthTreeFramesRollupCycle(t *testing.T) {
	rows := []healthTreeMonitorRow{
		monitorRow("a", "health", "", "Entity Health", 2, "A"),
		monitorRow("a", "rollup", "health", "Rollup", 2, "A"),
		monitorRow("b", "health", "", "Entity Health", 2, "B"),
		monitorRow("b", "rollup", "health", "Rollup", 2, "B"),
	}
	links := []healthTreeLink{
		{SourceEntityID: "a", MonitorID: "rollup", TargetEntityID: "b", MemberMonitorID: "health"},
		{SourceEntityID: "b", MonitorID: "rollup", TargetEntityID: "a", MemberMonitorID: "health"},
	}
	v := viewHealthTree(t, healthTreeFrames(rows, links, "a", nil, false, false))
	v.assertNodes(t, "a:health", "a:rollup", "b:health", "b:rollup")
}

func TestHealthTreeFramesTruncation(t *testing.T) {
	t.Run("a tree over the node cap keeps its top and says so", func(t *testing.T) {
		rows := []healthTreeMonitorRow{monitorRow("e1", "health", "", "Entity Health", 1, "server01")}
		for i := 0; i < healthTreeMaxNodes+10; i++ {
			rows = append(rows, monitorRow("e1", fmt.Sprintf("m%05d", i), "health", fmt.Sprintf("Monitor %05d", i), 1, "server01"))
		}
		v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, false, false))
		if len(v.ids) != healthTreeMaxNodes {
			t.Errorf("got %d nodes, want the cap of %d", len(v.ids), healthTreeMaxNodes)
		}
		if v.ids[0] != "e1:health" {
			t.Errorf("first node = %s, want the root", v.ids[0])
		}
		v.assertConnected(t)
		if got := noticeTexts(v.notices); !strings.Contains(got, "too large to show in full") || !strings.Contains(got, "Unhealthy only") {
			t.Errorf("notices = %q, want a truncation warning suggesting Unhealthy only", got)
		}
	})

	t.Run("the query's own cut is reported too", func(t *testing.T) {
		rows := []healthTreeMonitorRow{monitorRow("e1", "health", "", "Entity Health", 3, "server01")}
		v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, true, true))
		got := noticeTexts(v.notices)
		if !strings.Contains(got, "too large to show in full") {
			t.Errorf("notices = %q, want a truncation warning", got)
		}
		if strings.Contains(got, "Turn on Unhealthy only") {
			t.Errorf("notices = %q, shouldn't suggest Unhealthy only when it's already on", got)
		}
	})
}

func TestHealthTreeFramesEmpty(t *testing.T) {
	for name, rows := range map[string][]healthTreeMonitorRow{
		"no rows": nil,
		// Rows that don't include the root's own top monitor have nothing to
		// hang from.
		"no root": {monitorRow("other", "health", "", "Entity Health", 1, "other")},
	} {
		t.Run(name, func(t *testing.T) {
			v := viewHealthTree(t, healthTreeFrames(rows, nil, "e1", nil, false, false))
			if len(v.ids) != 0 || len(v.edgeIDs) != 0 {
				t.Errorf("got %d nodes and %d edges, want none", len(v.ids), len(v.edgeIDs))
			}
		})
	}
}
