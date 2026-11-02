package scom

import (
	"database/sql"
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
			if !IsBadRequest(err) {
				t.Errorf("aggregationTable(%q): got %v, want a bad request error", c.agg, err)
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

func TestPerfValueColumn(t *testing.T) {
	cases := []struct {
		agg  Aggregation
		stat Statistic
		want string
	}{
		{AggregationHourly, "", "AverageValue"}, // unset keeps the old behaviour
		{AggregationHourly, StatisticAverage, "AverageValue"},
		{AggregationHourly, StatisticMin, "MinValue"},
		{AggregationDaily, StatisticMax, "MaxValue"},
		// A raw row is one sample, so every statistic is the sample.
		{AggregationRaw, StatisticMax, "SampleValue"},
		{"", StatisticMin, "SampleValue"},
	}
	for _, c := range cases {
		got, err := perfValueColumn(c.agg, c.stat)
		if err != nil || got != c.want {
			t.Errorf("perfValueColumn(%q, %q) = (%q, %v), want %q", c.agg, c.stat, got, err, c.want)
		}
	}
	if _, err := perfValueColumn(AggregationHourly, "median"); !IsBadRequest(err) {
		t.Errorf("got %v for an unknown statistic, want a bad request error", err)
	}
}

func TestBuildPerformanceQuery(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	t.Run("hourly aggregation reads AverageValue from vPerfHourly, ids only", func(t *testing.T) {
		query, args, err := buildPerformanceQuery([]int64{1}, "", "", nil, AggregationHourly, "", from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{
			"FROM Perf.vPerfHourly p",
			"p.AverageValue AS Value",
			"p.PerformanceRuleInstanceRowId,",
			"p.ManagedEntityRowId,",
			"p.DateTime,",
			"WHERE p.PerformanceRuleInstanceRowId IN (@ctr0)",
			"AND p.DateTime >= @from AND p.DateTime <= @to",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}
		// Names come from buildPerformanceSeriesQuery, once per series —
		// not repeated on every fact row.
		for _, avoid := range []string{"ObjectName", "ManagedEntityDefaultName", "JOIN", "ORDER BY", "#EntityScope"} {
			if strings.Contains(query, avoid) {
				t.Errorf("query should not contain %q\nfull query:\n%s", avoid, query)
			}
		}
		if len(args) != 3 { // from, to, ctr0
			t.Fatalf("got %d args, want 3", len(args))
		}
		if v := args[2].(sql.NamedArg).Value; v != int64(1) {
			t.Errorf("counter id bound as %T %v, want int64 1", v, v)
		}
	})

	t.Run("statistic picks the rollup column", func(t *testing.T) {
		query, _, err := buildPerformanceQuery([]int64{1}, "", "", nil, AggregationDaily, StatisticMax, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "FROM Perf.vPerfDaily p") || !strings.Contains(query, "p.MaxValue AS Value") {
			t.Errorf("query should read MaxValue from Perf.vPerfDaily:\n%s", query)
		}
	})

	t.Run("raw aggregation selects SampleValue from vPerfRaw", func(t *testing.T) {
		query, _, err := buildPerformanceQuery([]int64{1}, "", "", nil, AggregationRaw, "", from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "FROM Perf.vPerfRaw p") || !strings.Contains(query, "p.SampleValue AS Value") {
			t.Errorf("query should read SampleValue from Perf.vPerfRaw:\n%s", query)
		}
	})

	t.Run("entityIDs scope the fact rows through temp tables, not a literal IN list", func(t *testing.T) {
		query, args, err := buildPerformanceQuery([]int64{1}, "", "", []string{"ent-1", "ent-2"}, AggregationHourly, "", from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{
			"CREATE TABLE #EntityScope",
			"INSERT INTO #EntityScope",
			// guids are turned into the fact view's own row ids once, so the
			// big view is filtered on its own column.
			"CREATE TABLE #EntityRows (Id int PRIMARY KEY)",
			"INNER JOIN #EntityScope s ON s.Id = me.ManagedEntityGuid",
			"AND p.ManagedEntityRowId IN (SELECT Id FROM #EntityRows)",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}
		// from, to, ctr0, plus exactly one bound param for the entity scope
		// regardless of how many entity ids there are — see idScopeTempTable.
		if len(args) != 4 {
			t.Fatalf("got %d args, want 4", len(args))
		}
	})

	t.Run("no counter ids matches by object and counter name through a temp table", func(t *testing.T) {
		query, args, err := buildPerformanceQuery(nil, "Processor", "% Processor Time", nil, AggregationHourly, "", from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{
			"CREATE TABLE #CounterRows (Id int PRIMARY KEY)",
			"WHERE pr.ObjectName = @object AND pr.CounterName = @counterName",
			"p.PerformanceRuleInstanceRowId IN (SELECT Id FROM #CounterRows)",
		} {
			if !strings.Contains(query, want) {
				t.Errorf("query missing %q\nfull query:\n%s", want, query)
			}
		}
		if strings.Contains(query, "@ctr0") {
			t.Errorf("query should not use a counter id IN list:\n%s", query)
		}
		if len(args) != 4 { // from, to, object, counterName
			t.Fatalf("got %d args, want 4", len(args))
		}
	})

	t.Run("invalid aggregation errors instead of building a bad query", func(t *testing.T) {
		if _, _, err := buildPerformanceQuery([]int64{1}, "", "", nil, "weekly", "", from, to); !IsBadRequest(err) {
			t.Fatalf("got %v for an unsupported aggregation, want a bad request error", err)
		}
	})
}

func TestBuildPerformanceSeriesQuery(t *testing.T) {
	query, args := buildPerformanceSeriesQuery([]int64{1, 2}, []int64{10})
	for _, want := range []string{
		"CREATE TABLE #SeriesCounters (Id int PRIMARY KEY)",
		"FROM STRING_SPLIT(@seriesCounterIds, ',')",
		"CREATE TABLE #SeriesEntities (Id int PRIMARY KEY)",
		"FROM STRING_SPLIT(@seriesEntityIds, ',')",
		"INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId",
		"WHERE pri.PerformanceRuleInstanceRowId IN (SELECT Id FROM #SeriesCounters)",
		"LEFT JOIN dbo.vManagedEntity tme ON tme.ManagedEntityRowId = COALESCE(me.TopLevelHostManagedEntityRowId, me.ManagedEntityRowId)",
		// ManagedEntityDefaultName is a computed alias of the nullable DisplayName.
		"COALESCE(me.ManagedEntityDefaultName, me.Name, CONVERT(nvarchar(64), me.ManagedEntityGuid))",
		"WHERE me.ManagedEntityRowId IN (SELECT Id FROM #SeriesEntities)",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q\nfull query:\n%s", want, query)
		}
	}
	if len(args) != 2 {
		t.Fatalf("got %d args, want one per id list", len(args))
	}
}

func TestPerfFrames(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	series := map[perfSeriesKey][]perfPoint{
		{counterRowID: 1, entityRowID: 20}: {{t0.Add(time.Hour), 2}, {t0, 1}}, // out of order on purpose
		{counterRowID: 1, entityRowID: 10}: {{t0, 5}},
	}
	counters := map[int64]perfCounterMeta{1: {object: "LogicalDisk", counter: "% Free Space", instance: "C:"}}
	entities := map[int64]perfEntityMeta{
		10: {guid: "guid-10", name: "zeta.contoso.com", host: "zeta.contoso.com"},
		20: {guid: "guid-20", name: "alpha.contoso.com", host: "alpha.contoso.com"},
	}

	frames, first, last := perfFrames(series, counters, entities, "", false)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}

	t.Run("frames are sorted by legend", func(t *testing.T) {
		if frames[0].Name != "LogicalDisk - % Free Space [C:] (alpha.contoso.com)" {
			t.Errorf("first frame is %q, want the alpha series", frames[0].Name)
		}
	})

	t.Run("points are in time order", func(t *testing.T) {
		times := frames[0].Fields[0]
		if times.At(0).(time.Time) != t0 || times.At(1).(time.Time) != t0.Add(time.Hour) {
			t.Errorf("times out of order: %v, %v", times.At(0), times.At(1))
		}
		if v := frames[0].Fields[1].At(0); v != 1.0 {
			t.Errorf("first value %v, want 1 (sorted with its timestamp)", v)
		}
	})

	t.Run("labels carry the names and ids", func(t *testing.T) {
		want := data.Labels{
			"object": "LogicalDisk", "counter": "% Free Space", "instance": "C:",
			"entity": "alpha.contoso.com", "host": "alpha.contoso.com",
			"counterId": "1", "entityId": "guid-20",
		}
		if got := frames[0].Fields[1].Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("got labels %v, want %v", got, want)
		}
	})

	t.Run("frames declare timeseries-multi", func(t *testing.T) {
		if frames[0].Meta == nil || frames[0].Meta.Type != data.FrameTypeTimeSeriesMulti {
			t.Errorf("got meta %+v, want timeseries-multi", frames[0].Meta)
		}
	})

	t.Run("reports the first and last sample", func(t *testing.T) {
		if first != t0 || last != t0.Add(time.Hour) {
			t.Errorf("got first=%v last=%v", first, last)
		}
	})

	t.Run("filterable value adds the hidden copy", func(t *testing.T) {
		frames, _, _ := perfFrames(series, counters, entities, "", true)
		if len(frames[0].Fields) != 3 {
			t.Errorf("got %d fields, want 3", len(frames[0].Fields))
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
}

func TestFilterableValueField(t *testing.T) {
	values := []float64{42, 41}
	field := filterableValueField(values)

	// "Filter data by values" resolves its target by display name, so this
	// has to read the same in every frame — hence Config.DisplayName, which
	// calculateFieldDisplayName returns before it would prefix the frame's
	// name (the series' legend) onto it.
	t.Run("display name is stable across frames", func(t *testing.T) {
		if field.Config == nil || field.Config.DisplayName != "value" {
			t.Fatalf("got Config %+v, want DisplayName \"value\"", field.Config)
		}
	})

	// A second numeric field would otherwise draw an identical second line
	// per series and double the legend.
	t.Run("hidden from the graph", func(t *testing.T) {
		hideFrom, ok := field.Config.Custom["hideFrom"].(map[string]any)
		if !ok {
			t.Fatalf("got Custom %+v, want a hideFrom entry", field.Config.Custom)
		}
		for _, k := range []string{"viz", "legend", "tooltip"} {
			if hideFrom[k] != true {
				t.Errorf("hideFrom[%q] = %v, want true", k, hideFrom[k])
			}
		}
	})

	t.Run("carries the same values", func(t *testing.T) {
		if field.Len() != len(values) {
			t.Fatalf("got %d values, want %d", field.Len(), len(values))
		}
		for i, want := range values {
			if got := field.At(i); got != want {
				t.Errorf("value[%d] = %v, want %v", i, got, want)
			}
		}
	})

	// Copied rather than aliased, so a later edit to either slice can't
	// silently desynchronise the graphed series from the filterable one.
	t.Run("does not alias the original slice", func(t *testing.T) {
		values[0] = 99
		if field.At(0) == 99.0 {
			t.Error("copy shares backing array with the original values")
		}
	})

	// No labels: labels are what make a field a distinct series, and this one
	// is deliberately not a series of its own.
	t.Run("carries no labels", func(t *testing.T) {
		if len(field.Labels) != 0 {
			t.Errorf("got labels %v, want none", field.Labels)
		}
	})
}
