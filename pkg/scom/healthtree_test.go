package scom

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildHealthTreeQuery(t *testing.T) {
	query, args := buildHealthTreeQuery([]string{"entity-1"})

	wantContain := []string{
		"FROM dbo.State s",
		"INNER JOIN dbo.Monitor m ON m.MonitorId = s.MonitorId",
		"INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId",
		// The one thing dbo.MonitorView supplied that the base table doesn't,
		// and so has to be carried over by hand.
		"AND mp.ContentReadable = 1",
		// A monitor localized into some other language, or carrying no
		// display string at all, still has to reach the tree — see
		// localizedNameApply.
		"WHERE lt.LTStringId = m.MonitorId AND lt.LTStringType = 1",
		"ISNULL(disp.LTValue, m.MonitorName) AS DisplayName",
		// (MonitorId, HealthState) is not unique in
		// dbo.MonitorOperationalState, so a plain LEFT JOIN fans out and
		// duplicates node ids — see buildHealthTreeQuery.
		"OUTER APPLY (",
		"SELECT TOP 1 mosi.MonitorOperationalStateName",
		"WHERE mosi.MonitorId = s.MonitorId AND mosi.HealthState = s.HealthState",
		// Proves healthStateCase is reused rather than reimplemented.
		"ISNULL(mos.MonitorOperationalStateName, CASE s.HealthState",
		"LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId",
		"WHERE s.BaseManagedEntityId IN (SELECT Id FROM #HealthTreeScope)",
		// A monitor guid repeats across entities, so the entity has to come
		// back on every row to build a unique node id — see healthTreeNodeID.
		"CONVERT(varchar(64), s.BaseManagedEntityId) AS EntityId",
	}
	for _, want := range wantContain {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}

	// The one thing that must differ from buildHealthCurrentQuery: a health
	// tree wants every monitor for the entity, not just the top-level
	// rollup, so it must not filter down to the single rollup monitor.
	if strings.Contains(query, "fn_ManagedTypeId_SystemHealthEntityState()") {
		t.Errorf("query should not filter to the single rollup monitor\nfull query:\n%s", query)
	}

	// Pinning one LanguageCode is what used to drop monitors localized into
	// any other language (this deployment runs ENU and ENA), and monitors
	// with no display string at all, whose LanguageCode is NULL and so
	// matches nothing. Language is a preference now, not a filter.
	if strings.Contains(query, "LanguageCode = '") {
		t.Errorf("query filters on a single LanguageCode, want a preference order\nfull query:\n%s", query)
	}
	if !strings.Contains(query, "CASE lt.LanguageCode WHEN 'ENU' THEN 0 WHEN 'ENA' THEN 1 ELSE 2 END") {
		t.Errorf("query missing the ENU/ENA language preference order\nfull query:\n%s", query)
	}

	// A group tree's entity list is every member, which is exactly the size
	// that has to go through the temp table rather than a literal IN list.
	t.Run("many entities still bind a single parameter", func(t *testing.T) {
		ids := make([]string, 0, 3000)
		for i := 0; i < 3000; i++ {
			ids = append(ids, fmt.Sprintf("entity-%d", i))
		}
		_, manyArgs := buildHealthTreeQuery(ids)
		if len(manyArgs) != 1 {
			t.Errorf("got %d args for 3000 entities, want 1 (SQL Server caps an RPC at ~2,100)", len(manyArgs))
		}
	})

	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
	if _, ok := args[0].(sql.NamedArg); !ok {
		t.Errorf("got args=%v, want a single named arg", args)
	}
}

func TestHealthTreeFrames(t *testing.T) {
	lastModified := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	rows := []healthTreeMonitorRow{
		{
			// Root: no parent, healthy.
			EntityID:          "e1",
			MonitorID:         "root",
			ParentMonitorID:   sql.NullString{},
			DisplayName:       "Entity Health",
			HealthState:       1,
			HealthStateName:   "Healthy",
			LastModified:      lastModified,
			EntityDisplayName: "server01.contoso.example.com",
		},
		{
			// Child of root: warning.
			EntityID:          "e1",
			MonitorID:         "availability",
			ParentMonitorID:   sql.NullString{String: "root", Valid: true},
			DisplayName:       "Availability",
			Category:          sql.NullString{String: "AvailabilityHealth", Valid: true},
			HealthState:       2,
			HealthStateName:   "Warning",
			LastModified:      lastModified,
			EntityDisplayName: "server01.contoso.example.com",
		},
		{
			// Grandchild: critical, in maintenance mode.
			EntityID:          "e1",
			MonitorID:         "disk-c",
			ParentMonitorID:   sql.NullString{String: "availability", Valid: true},
			DisplayName:       "Logical Disk Free Space",
			HealthState:       3,
			HealthStateName:   "Critical",
			LastModified:      lastModified,
			InMaintenance:     true,
			EntityDisplayName: "server01.contoso.example.com",
		},
		{
			// Dangling parent: references a monitor not present in this
			// entity's rows at all — must not produce an edge.
			EntityID:          "e1",
			MonitorID:         "orphan",
			ParentMonitorID:   sql.NullString{String: "does-not-exist", Valid: true},
			DisplayName:       "Orphaned Monitor",
			HealthState:       0,
			HealthStateName:   "Not Monitored",
			LastModified:      lastModified,
			EntityDisplayName: "server01.contoso.example.com",
		},
	}

	frames := healthTreeFrames(rows, "e1")
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (nodes, edges)", len(frames))
	}
	nodes, edges := frames[0], frames[1]

	if nodes.Name != "nodes" || edges.Name != "edges" {
		t.Fatalf("got frame names %q, %q, want \"nodes\", \"edges\"", nodes.Name, edges.Name)
	}

	for _, f := range frames {
		if f.Meta == nil || f.Meta.PreferredVisualization != "nodeGraph" {
			t.Errorf("frame %q missing PreferredVisualization=nodeGraph: %+v", f.Name, f.Meta)
		}
	}

	t.Run("node count matches input rows and root title is the entity name", func(t *testing.T) {
		idField, _ := nodes.FieldByName("id")
		if idField == nil || idField.Len() != len(rows) {
			t.Fatalf("got %v nodes, want %d", idField, len(rows))
		}
		titleField, _ := nodes.FieldByName("title")
		if got := titleField.At(0); got != "server01.contoso.example.com" {
			t.Errorf("root node title = %v, want the entity's own display name, not the monitor name", got)
		}
		if got := titleField.At(1); got != "Availability" {
			t.Errorf("non-root node title = %v, want the monitor's own display name", got)
		}
	})

	t.Run("arc fields are one-hot and sum to 1 per node", func(t *testing.T) {
		wantArc := map[string][]float64{
			"arc__healthy":      {1, 0, 0, 0},
			"arc__warning":      {0, 1, 0, 0},
			"arc__critical":     {0, 0, 1, 0},
			"arc__notmonitored": {0, 0, 0, 1},
		}
		for name, want := range wantArc {
			f, _ := nodes.FieldByName(name)
			if f == nil {
				t.Fatalf("missing arc field %q", name)
			}
			for i, w := range want {
				if got := f.At(i).(float64); got != w {
					t.Errorf("%s[%d] = %v, want %v", name, i, got, w)
				}
			}
		}
	})

	t.Run("edges skip the root (no parent) and the dangling parent", func(t *testing.T) {
		idField, _ := edges.FieldByName("id")
		sourceField, _ := edges.FieldByName("source")
		targetField, _ := edges.FieldByName("target")
		if idField.Len() != 2 {
			t.Fatalf("got %d edges, want 2 (availability->root, disk-c->availability)", idField.Len())
		}
		if sourceField.At(0) != "e1:root" || targetField.At(0) != "e1:availability" {
			t.Errorf("edge 0 = %v -> %v, want e1:root -> e1:availability", sourceField.At(0), targetField.At(0))
		}
		if sourceField.At(1) != "e1:availability" || targetField.At(1) != "e1:disk-c" {
			t.Errorf("edge 1 = %v -> %v, want e1:availability -> e1:disk-c", sourceField.At(1), targetField.At(1))
		}
	})

	t.Run("empty input produces empty, but valid, frames", func(t *testing.T) {
		empty := healthTreeFrames(nil, "e1")
		if len(empty) != 2 {
			t.Fatalf("got %d frames, want 2", len(empty))
		}
	})
}

// The bug this guards: keyed on the monitor guid alone, every member's copy
// of a shared monitor collapsed into one node, so a group rendered as a
// single machine's tree no matter how many members it had.
func TestHealthTreeFramesGroup(t *testing.T) {
	// group "g" with two members, each carrying the *same* monitor guids
	// ("root", "disk") as every other Windows computer does.
	rows := []healthTreeMonitorRow{
		{EntityID: "g", MonitorID: "root", DisplayName: "Entity Health", HealthState: 2, EntityDisplayName: "All Servers"},
		{EntityID: "m1", MonitorID: "root", DisplayName: "Entity Health", HealthState: 1, EntityDisplayName: "server01"},
		{EntityID: "m1", MonitorID: "disk", ParentMonitorID: sql.NullString{String: "root", Valid: true}, DisplayName: "Disk", HealthState: 1, EntityDisplayName: "server01"},
		{EntityID: "m2", MonitorID: "root", DisplayName: "Entity Health", HealthState: 3, EntityDisplayName: "server02"},
		{EntityID: "m2", MonitorID: "disk", ParentMonitorID: sql.NullString{String: "root", Valid: true}, DisplayName: "Disk", HealthState: 3, EntityDisplayName: "server02"},
	}

	frames := healthTreeFrames(rows, "g")
	nodes, edges := frames[0], frames[1]

	t.Run("every row is its own node despite shared monitor ids", func(t *testing.T) {
		idField, _ := nodes.FieldByName("id")
		if idField.Len() != len(rows) {
			t.Fatalf("got %d nodes, want %d", idField.Len(), len(rows))
		}
		seen := map[string]bool{}
		for i := 0; i < idField.Len(); i++ {
			id := idField.At(i).(string)
			if seen[id] {
				t.Errorf("duplicate node id %q — members would collapse into one node", id)
			}
			seen[id] = true
		}
	})

	t.Run("each member's root title is its own machine name", func(t *testing.T) {
		titleField, _ := nodes.FieldByName("title")
		want := []string{"All Servers", "server01", "Disk", "server02", "Disk"}
		for i, w := range want {
			if got := titleField.At(i); got != w {
				t.Errorf("title[%d] = %v, want %v", i, got, w)
			}
		}
	})

	t.Run("member roots are grafted onto the group root", func(t *testing.T) {
		sourceField, _ := edges.FieldByName("source")
		targetField, _ := edges.FieldByName("target")
		got := map[string]string{}
		for i := 0; i < sourceField.Len(); i++ {
			got[targetField.At(i).(string)] = sourceField.At(i).(string)
		}
		want := map[string]string{
			"m1:root": "g:root",
			"m2:root": "g:root",
			"m1:disk": "m1:root",
			"m2:disk": "m2:root",
		}
		if len(got) != len(want) {
			t.Fatalf("got %d edges %v, want %d %v", len(got), got, len(want), want)
		}
		for target, wantSource := range want {
			if got[target] != wantSource {
				t.Errorf("edge into %s came from %q, want %q", target, got[target], wantSource)
			}
		}
	})
}

func TestFilterUnhealthyBranches(t *testing.T) {
	// root -> availability (healthy) -> disk-c (critical)
	// root -> performance (healthy) -> counter-x (healthy)
	// A sibling branch (performance/counter-x) that's entirely healthy must
	// be dropped, while the ancestor chain above the critical monitor
	// (root, availability) must survive even though root/availability are
	// healthy themselves.
	rows := []healthTreeMonitorRow{
		{EntityID: "e1", MonitorID: "root", HealthState: 1},
		{EntityID: "e1", MonitorID: "availability", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 1},
		{EntityID: "e1", MonitorID: "disk-c", ParentMonitorID: sql.NullString{String: "availability", Valid: true}, HealthState: 3},
		{EntityID: "e1", MonitorID: "performance", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 1},
		{EntityID: "e1", MonitorID: "counter-x", ParentMonitorID: sql.NullString{String: "performance", Valid: true}, HealthState: 1},
		{EntityID: "e1", MonitorID: "not-monitored", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 0},
	}

	got := filterUnhealthyBranches(rows, "e1")

	gotIDs := make(map[string]bool, len(got))
	for _, r := range got {
		gotIDs[r.MonitorID] = true
	}
	wantIDs := []string{"root", "availability", "disk-c"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d rows %v, want %d rows %v", len(got), gotIDs, len(wantIDs), wantIDs)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("missing expected row %q in filtered result %v", id, gotIDs)
		}
	}

	t.Run("all-healthy tree filters down to nothing", func(t *testing.T) {
		allHealthy := []healthTreeMonitorRow{
			{EntityID: "e1", MonitorID: "root", HealthState: 1},
			{EntityID: "e1", MonitorID: "availability", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 1},
		}
		if got := filterUnhealthyBranches(allHealthy, "e1"); len(got) != 0 {
			t.Errorf("got %d rows, want 0", len(got))
		}
	})

	// The whole point of pruning in a group tree: a member with nothing
	// wrong contributes no nodes at all, while an unhealthy member keeps its
	// chain and drags the group root along with it.
	t.Run("healthy members are dropped whole, unhealthy ones keep the group root", func(t *testing.T) {
		group := []healthTreeMonitorRow{
			{EntityID: "g", MonitorID: "root", HealthState: 1},
			{EntityID: "m1", MonitorID: "root", HealthState: 1},
			{EntityID: "m1", MonitorID: "disk", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 1},
			{EntityID: "m2", MonitorID: "root", HealthState: 1},
			{EntityID: "m2", MonitorID: "disk", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 3},
		}

		kept := map[string]bool{}
		for _, r := range filterUnhealthyBranches(group, "g") {
			kept[healthTreeNodeID(r.EntityID, r.MonitorID)] = true
		}

		want := []string{"g:root", "m2:root", "m2:disk"}
		if len(kept) != len(want) {
			t.Fatalf("got %d rows %v, want %d %v", len(kept), kept, len(want), want)
		}
		for _, id := range want {
			if !kept[id] {
				t.Errorf("missing %q — the group root must survive via the unhealthy member's chain", id)
			}
		}
		for _, id := range []string{"m1:root", "m1:disk"} {
			if kept[id] {
				t.Errorf("%q survived, but m1 is entirely healthy and should be dropped whole", id)
			}
		}
	})
}

func TestFilterMonitored(t *testing.T) {
	rows := []healthTreeMonitorRow{
		{EntityID: "e1", MonitorID: "root", HealthState: 1},
		{EntityID: "e1", MonitorID: "disabled-child", ParentMonitorID: sql.NullString{String: "root", Valid: true}, HealthState: 0},
		{EntityID: "e1", MonitorID: "disk-c", ParentMonitorID: sql.NullString{String: "disabled-child", Valid: true}, HealthState: 3},
	}

	got := filterMonitored(rows)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (root, disk-c)", len(got))
	}
	for _, r := range got {
		if r.MonitorID == "disabled-child" {
			t.Errorf("Not Monitored row %q should have been dropped", r.MonitorID)
		}
	}

	// Dropping "disabled-child" orphans disk-c's parent link — healthTreeFrames
	// already tolerates this (see the "dangling parent" case in
	// TestHealthTreeFrames), so disk-c should still come through as a node,
	// just without an edge back to root.
	frames := healthTreeFrames(got, "e1")
	nodeIDField, _ := frames[0].FieldByName("id")
	if nodeIDField.Len() != 2 {
		t.Fatalf("got %d nodes, want 2", nodeIDField.Len())
	}
	edgeIDField, _ := frames[1].FieldByName("id")
	if edgeIDField.Len() != 0 {
		t.Errorf("got %d edges, want 0 since disk-c's parent was dropped", edgeIDField.Len())
	}
}
