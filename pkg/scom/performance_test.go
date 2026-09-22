package scom

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

func TestAggregationTable(t *testing.T) {
	cases := []struct {
		agg        Aggregation
		wantTable  string
		wantColumn string
		wantErr    bool
	}{
		{AggregationRaw, "Perf.vPerfRaw", "SampleValue", false},
		{"", "Perf.vPerfRaw", "SampleValue", false}, // unset defaults to raw
		{AggregationHourly, "Perf.vPerfHourly", "AverageValue", false},
		{AggregationDaily, "Perf.vPerfDaily", "AverageValue", false},
		{"weekly", "", "", true},
	}
	for _, c := range cases {
		table, col, err := aggregationTable(c.agg)
		if c.wantErr {
			if err == nil {
				t.Errorf("aggregationTable(%q): got no error, want one", c.agg)
			}
			continue
		}
		if err != nil {
			t.Errorf("aggregationTable(%q): unexpected error: %v", c.agg, err)
		}
		if table != c.wantTable || col != c.wantColumn {
			t.Errorf("aggregationTable(%q) = (%q, %q), want (%q, %q)", c.agg, table, col, c.wantTable, c.wantColumn)
		}
	}
}

func TestBuildPerformanceQuery(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("hourly aggregation selects AverageValue from vPerfHourly", func(t *testing.T) {
		query, args, err := buildPerformanceQuery([]string{"ctr-1"}, "", "", nil, AggregationHourly, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "FROM Perf.vPerfHourly p") {
			t.Errorf("query missing Perf.vPerfHourly: %s", query)
		}
		if !strings.Contains(query, "p.AverageValue AS Value") {
			t.Errorf("query missing AverageValue column: %s", query)
		}
		if !strings.Contains(query, "INNER JOIN dbo.vPerformanceRuleInstance pri ON p.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId") ||
			!strings.Contains(query, "INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId") ||
			!strings.Contains(query, "INNER JOIN dbo.vManagedEntity me ON p.ManagedEntityRowId = me.ManagedEntityRowId") ||
			!strings.Contains(query, "LEFT JOIN dbo.vManagedEntity tme ON tme.ManagedEntityRowId = COALESCE(me.TopLevelHostManagedEntityRowId, me.ManagedEntityRowId)") {
			t.Errorf("query missing expected joins: %s", query)
		}
		if !strings.Contains(query, "tme.ManagedEntityDefaultName AS HostDisplayName") {
			t.Errorf("query missing top-level host display name column: %s", query)
		}
		if !strings.Contains(query, "CONVERT(varchar(64), me.ManagedEntityGuid) AS ManagedEntityGuid") {
			t.Errorf("query missing managed entity guid column: %s", query)
		}
		if !strings.Contains(query, "WHERE p.PerformanceRuleInstanceRowId IN (@ctr0)") {
			t.Errorf("query missing counter id filter: %s", query)
		}
		if strings.Contains(query, "ManagedEntityGuid IN") {
			t.Errorf("query should not filter by entity when entityIDs is empty: %s", query)
		}
		if strings.Contains(query, "#EntityScope") {
			t.Errorf("query should not set up an entity scope temp table when entityIDs is empty: %s", query)
		}
		if len(args) != 3 { // from, to, ctr0
			t.Fatalf("got %d args, want 3", len(args))
		}
	})

	t.Run("raw aggregation selects SampleValue from vPerfRaw", func(t *testing.T) {
		query, _, err := buildPerformanceQuery([]string{"ctr-1"}, "", "", nil, AggregationRaw, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "FROM Perf.vPerfRaw p") || !strings.Contains(query, "p.SampleValue AS Value") {
			t.Errorf("query should read SampleValue from Perf.vPerfRaw: %s", query)
		}
	})

	t.Run("entityIDs scopes results via a temp table join, not a literal IN list", func(t *testing.T) {
		query, args, err := buildPerformanceQuery([]string{"ctr-1"}, "", "", []string{"ent-1", "ent-2"}, AggregationHourly, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "CREATE TABLE #EntityScope") {
			t.Errorf("query missing entity scope temp table creation: %s", query)
		}
		if !strings.Contains(query, "INSERT INTO #EntityScope") {
			t.Errorf("query missing entity scope temp table population: %s", query)
		}
		if !strings.Contains(query, "AND me.ManagedEntityGuid IN (SELECT Id FROM #EntityScope)") {
			t.Errorf("query missing entity id filter joined against the temp table: %s", query)
		}
		// from, to, ctr0, plus exactly one bound param for the entity scope
		// regardless of how many entity ids there are — see idScopeTempTable.
		if len(args) != 4 {
			t.Fatalf("got %d args, want 4", len(args))
		}
	})

	t.Run("no counter ids matches by object and counter name, with no IN list", func(t *testing.T) {
		query, args, err := buildPerformanceQuery(nil, "Processor", "% Processor Time", nil, AggregationHourly, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "WHERE pr.ObjectName = @object AND pr.CounterName = @counterName") {
			t.Errorf("query missing object/counter name filter: %s", query)
		}
		if strings.Contains(query, "PerformanceRuleInstanceRowId IN") {
			t.Errorf("query should not use a counter id IN list: %s", query)
		}
		if len(args) != 4 { // from, to, object, counterName
			t.Fatalf("got %d args, want 4", len(args))
		}
	})

	t.Run("invalid aggregation errors instead of building a bad query", func(t *testing.T) {
		_, _, err := buildPerformanceQuery([]string{"ctr-1"}, "", "", nil, "weekly", from, to)
		if err == nil {
			t.Fatal("got no error for an unsupported aggregation, want one")
		}
	})
}

func TestSeriesLabel(t *testing.T) {
	t.Run("empty legendFormat falls back to the built-in default", func(t *testing.T) {
		got := seriesLabel("", "LogicalDisk", "% Free Space", "C:", "server01.contoso.example.com", "server01.contoso.example.com")
		want := "LogicalDisk - % Free Space [C:] (server01.contoso.example.com)"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("empty legendFormat omits the instance segment when there's no instance", func(t *testing.T) {
		got := seriesLabel("", "Memory", "Available Bytes", "", "server01.contoso.example.com", "server01.contoso.example.com")
		want := "Memory - Available Bytes (server01.contoso.example.com)"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legendFormat substitutes every macro", func(t *testing.T) {
		got := seriesLabel("{{entity}} on {{host}}: {{object}}/{{counter}} [{{instance}}]", "LogicalDisk", "% Free Space", "C:", "LogicalDisk C:", "server01")
		want := "LogicalDisk C: on server01: LogicalDisk/% Free Space [C:]"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legendFormat with no macros passes through unchanged", func(t *testing.T) {
		got := seriesLabel("fixed label", "LogicalDisk", "% Free Space", "C:", "server01", "server01")
		if got != "fixed label" {
			t.Errorf("got %q, want %q", got, "fixed label")
		}
	})

	t.Run("legendFormat referencing a macro for missing data substitutes empty string", func(t *testing.T) {
		got := seriesLabel("{{object}} [{{instance}}]", "Memory", "Available Bytes", "", "server01", "server01")
		want := "Memory []"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("empty hostName falls back to entityName", func(t *testing.T) {
		got := seriesLabel("{{host}}", "LogicalDisk", "% Free Space", "C:", "server01", "")
		if got != "server01" {
			t.Errorf("got %q, want %q", got, "server01")
		}
	})
}

func TestSeriesLabels(t *testing.T) {
	t.Run("carries every dimension the legend macros expose", func(t *testing.T) {
		got := seriesLabels("ctr-1", "ent-1", "LogicalDisk", "% Free Space", "C:", "LogicalDisk C: server01", "server01.contoso.example.com")
		want := data.Labels{
			"object":    "LogicalDisk",
			"counter":   "% Free Space",
			"instance":  "C:",
			"entity":    "LogicalDisk C: server01",
			"host":      "server01.contoso.example.com",
			"counterId": "ctr-1",
			"entityId":  "ent-1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty hostName falls back to entityName, same as the legend", func(t *testing.T) {
		got := seriesLabels("ctr-1", "ent-1", "LogicalDisk", "% Free Space", "C:", "server01", "")
		if got["host"] != "server01" {
			t.Errorf("got host %q, want %q", got["host"], "server01")
		}
	})

	// A response mixing instanced and non-instanced counters must not hand
	// Grafana frames with different label *keys* — see seriesLabels.
	t.Run("instance key is present even when there is no instance", func(t *testing.T) {
		got := seriesLabels("ctr-1", "ent-1", "Memory", "Available Bytes", "", "server01", "server01")
		v, ok := got["instance"]
		if !ok {
			t.Fatal("instance label is missing entirely, want it present and empty")
		}
		if v != "" {
			t.Errorf("got instance %q, want empty", v)
		}
	})

	// Display names are not unique (two cloned agents share one, two MPs can
	// define the same object+counter), so the ids are what keep two genuinely
	// distinct series from colliding under the timeseries-multi contract.
	t.Run("series sharing every display name stay distinct via the ids", func(t *testing.T) {
		a := seriesLabels("ctr-1", "ent-1", "LogicalDisk", "% Free Space", "C:", "server01", "server01")
		b := seriesLabels("ctr-1", "ent-2", "LogicalDisk", "% Free Space", "C:", "server01", "server01")
		if reflect.DeepEqual(a, b) {
			t.Error("two entities sharing a display name produced identical label sets, want distinct")
		}

		c := seriesLabels("ctr-2", "ent-1", "LogicalDisk", "% Free Space", "C:", "server01", "server01")
		if reflect.DeepEqual(a, c) {
			t.Error("two rules sharing an object+counter produced identical label sets, want distinct")
		}
	})

	// The division of labour: the ids make the label set unique, but would
	// only be noise in a table, so they stay out of the columns.
	t.Run("ids are labels only, never columns", func(t *testing.T) {
		for _, d := range performanceDimensions {
			if d == "counterId" || d == "entityId" {
				t.Errorf("%q is emitted as a column, want it kept to labels", d)
			}
		}
	})
}

func testPerformanceSeries() []*performanceSeries {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	return []*performanceSeries{
		{
			label:  "LogicalDisk - % Free Space [C:] (server01)",
			labels: seriesLabels("ctr-1", "ent-1", "LogicalDisk", "% Free Space", "C:", "server01", "server01"),
			times:  []time.Time{t0, t0.Add(time.Hour)},
			values: []float64{42, 41},
		},
		{
			label:  "LogicalDisk - % Free Space [D:] (server01)",
			labels: seriesLabels("ctr-2", "ent-1", "LogicalDisk", "% Free Space", "D:", "server01", "server01"),
			// Deliberately different timestamps from the series above: agents
			// report on their own cadence.
			times:  []time.Time{t0.Add(30 * time.Minute)},
			values: []float64{88},
		},
	}
}

func TestPerformanceFramesTimeSeries(t *testing.T) {
	// The zero value is the default, so a query saved before the format
	// option existed keeps returning one frame per series.
	for _, format := range []PerformanceFormat{"", PerformanceFormatTimeSeries} {
		frames := performanceFrames(testPerformanceSeries(), format)
		if len(frames) != 2 {
			t.Fatalf("format %q: got %d frames, want one per series (2)", format, len(frames))
		}
		if frames[0].Meta == nil || frames[0].Meta.Type != data.FrameTypeTimeSeriesMulti {
			t.Errorf("format %q: frame meta = %+v, want timeseries-multi", format, frames[0].Meta)
		}
	}

	frames := performanceFrames(testPerformanceSeries(), PerformanceFormatTimeSeries)

	// Anything reading the shape positionally must still see the [time,
	// value] pair a plain timeseries-multi frame leads with — the dimension
	// columns go after, never between.
	t.Run("time and value stay the first two fields", func(t *testing.T) {
		want := []string{"time", "value", "object", "counter", "instance", "entity", "host"}
		for _, f := range frames {
			if len(f.Fields) != len(want) {
				t.Fatalf("got %d fields, want %d", len(f.Fields), len(want))
			}
			for i, name := range want {
				if f.Fields[i].Name != name {
					t.Errorf("field[%d] = %q, want %q", i, f.Fields[i].Name, name)
				}
			}
		}
	})

	t.Run("dimension columns repeat the series' own values down its length", func(t *testing.T) {
		instance, _ := frames[0].FieldByName("instance")
		if instance.Len() != 2 {
			t.Fatalf("got %d rows, want 2 to match the series' samples", instance.Len())
		}
		for i := 0; i < instance.Len(); i++ {
			if got := instance.At(i); got != "C:" {
				t.Errorf("instance[%d] = %v, want C:", i, got)
			}
		}
		if second, _ := frames[1].FieldByName("instance"); second.At(0) != "D:" {
			t.Errorf("second frame instance = %v, want D:", second.At(0))
		}
	})

	// The columns are additional to the labels, not a replacement, so an
	// existing ${__field.labels.x} override keeps working — and the legend
	// still comes from DisplayNameFromDS rather than an auto-generated dump.
	t.Run("labels and the rendered legend both survive", func(t *testing.T) {
		value, _ := frames[0].FieldByName("value")
		if value.Labels["instance"] != "C:" {
			t.Errorf("value labels = %v, want instance=C: still present", value.Labels)
		}
		if value.Config == nil || value.Config.DisplayNameFromDS != "LogicalDisk - % Free Space [C:] (server01)" {
			t.Errorf("got DisplayNameFromDS %+v, want the rendered legend", value.Config)
		}
	})
}

func TestPerformanceFramesTable(t *testing.T) {
	frames := performanceFrames(testPerformanceSeries(), PerformanceFormatTable)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want a single long frame", len(frames))
	}
	f := frames[0]

	t.Run("one row per sample across every series", func(t *testing.T) {
		if f.Rows() != 3 {
			t.Fatalf("got %d rows, want 3 (2 + 1 samples)", f.Rows())
		}
	})

	t.Run("dimensions are plain string columns, time and value survive", func(t *testing.T) {
		want := []string{"time", "object", "counter", "instance", "entity", "host", "value"}
		if len(f.Fields) != len(want) {
			t.Fatalf("got %d fields, want %d", len(f.Fields), len(want))
		}
		for i, name := range want {
			if f.Fields[i].Name != name {
				t.Errorf("field[%d] = %q, want %q", i, f.Fields[i].Name, name)
			}
		}
		for _, name := range performanceDimensions {
			field, _ := f.FieldByName(name)
			if field.Type() != data.FieldTypeString {
				t.Errorf("%s is %v, want a string column to filter on", name, field.Type())
			}
		}
	})

	t.Run("each row carries its own series' dimensions", func(t *testing.T) {
		instance, _ := f.FieldByName("instance")
		value, _ := f.FieldByName("value")
		wantInstance := []string{"C:", "C:", "D:"}
		wantValue := []float64{42, 41, 88}
		for i := range wantInstance {
			if got := instance.At(i); got != wantInstance[i] {
				t.Errorf("instance[%d] = %v, want %v", i, got, wantInstance[i])
			}
			if got := value.At(i); got != wantValue[i] {
				t.Errorf("value[%d] = %v, want %v", i, got, wantValue[i])
			}
		}
	})

	t.Run("no series still yields a valid empty frame", func(t *testing.T) {
		empty := performanceFrames(nil, PerformanceFormatTable)
		if len(empty) != 1 || empty[0].Rows() != 0 {
			t.Errorf("got %d frames, want one empty frame", len(empty))
		}
	})
}
