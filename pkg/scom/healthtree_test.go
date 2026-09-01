package scom

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestBuildHealthTreeQuery(t *testing.T) {
	query, args := buildHealthTreeQuery("entity-1")

	wantContain := []string{
		"FROM dbo.State s",
		"INNER JOIN dbo.MonitorView mv ON mv.Id = s.MonitorId",
		"INNER JOIN dbo.BaseManagedEntity bme ON bme.BaseManagedEntityId = s.BaseManagedEntityId",
		// dbo.MonitorView isn't pre-filtered to one language — same gotcha
		// classesQueryByDisplayName guards against — so omitting this
		// duplicates every row once per installed language pack.
		"AND mv.LanguageCode = 'ENU'",
		"LEFT JOIN dbo.MonitorOperationalState mos ON mos.MonitorId = s.MonitorId AND mos.HealthState = s.HealthState",
		// Proves healthStateCase is reused rather than reimplemented.
		"ISNULL(mos.MonitorOperationalStateName, CASE s.HealthState",
		"LEFT JOIN dbo.MaintenanceMode mm ON mm.BaseManagedEntityId = s.BaseManagedEntityId",
		"WHERE s.BaseManagedEntityId = @entityId",
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

	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
	named, ok := args[0].(sql.NamedArg)
	if !ok || named.Name != "entityId" || named.Value != "entity-1" {
		t.Errorf("got args=%v, want a single entityId=entity-1 arg", args)
	}
}

func TestHealthTreeFrames(t *testing.T) {
	lastModified := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	rows := []healthTreeMonitorRow{
		{
			// Root: no parent, healthy.
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
			MonitorID:         "orphan",
			ParentMonitorID:   sql.NullString{String: "does-not-exist", Valid: true},
			DisplayName:       "Orphaned Monitor",
			HealthState:       0,
			HealthStateName:   "Not Monitored",
			LastModified:      lastModified,
			EntityDisplayName: "server01.contoso.example.com",
		},
	}

	frames := healthTreeFrames(rows)
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
		if sourceField.At(0) != "root" || targetField.At(0) != "availability" {
			t.Errorf("edge 0 = %v -> %v, want root -> availability", sourceField.At(0), targetField.At(0))
		}
		if sourceField.At(1) != "availability" || targetField.At(1) != "disk-c" {
			t.Errorf("edge 1 = %v -> %v, want availability -> disk-c", sourceField.At(1), targetField.At(1))
		}
	})

	t.Run("empty input produces empty, but valid, frames", func(t *testing.T) {
		empty := healthTreeFrames(nil)
		if len(empty) != 2 {
			t.Fatalf("got %d frames, want 2", len(empty))
		}
	})
}
