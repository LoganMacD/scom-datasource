package scom

import (
	"strings"
	"testing"
	"time"
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
		query, args, err := buildPerformanceQuery([]string{"ctr-1"}, nil, AggregationHourly, from, to)
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
			!strings.Contains(query, "INNER JOIN dbo.vManagedEntity me ON p.ManagedEntityRowId = me.ManagedEntityRowId") {
			t.Errorf("query missing expected joins: %s", query)
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
		query, _, err := buildPerformanceQuery([]string{"ctr-1"}, nil, AggregationRaw, from, to)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(query, "FROM Perf.vPerfRaw p") || !strings.Contains(query, "p.SampleValue AS Value") {
			t.Errorf("query should read SampleValue from Perf.vPerfRaw: %s", query)
		}
	})

	t.Run("entityIDs scopes results via a temp table join, not a literal IN list", func(t *testing.T) {
		query, args, err := buildPerformanceQuery([]string{"ctr-1"}, []string{"ent-1", "ent-2"}, AggregationHourly, from, to)
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

	t.Run("invalid aggregation errors instead of building a bad query", func(t *testing.T) {
		_, _, err := buildPerformanceQuery([]string{"ctr-1"}, nil, "weekly", from, to)
		if err == nil {
			t.Fatal("got no error for an unsupported aggregation, want one")
		}
	})
}

func TestSeriesLabel(t *testing.T) {
	t.Run("empty legendFormat falls back to the built-in default", func(t *testing.T) {
		got := seriesLabel("", "LogicalDisk", "% Free Space", "C:", "server01.contoso.example.com")
		want := "LogicalDisk - % Free Space [C:] (server01.contoso.example.com)"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("empty legendFormat omits the instance segment when there's no instance", func(t *testing.T) {
		got := seriesLabel("", "Memory", "Available Bytes", "", "server01.contoso.example.com")
		want := "Memory - Available Bytes (server01.contoso.example.com)"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legendFormat substitutes every macro", func(t *testing.T) {
		got := seriesLabel("{{entity}}: {{object}}/{{counter}} [{{instance}}]", "LogicalDisk", "% Free Space", "C:", "server01")
		want := "server01: LogicalDisk/% Free Space [C:]"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("legendFormat with no macros passes through unchanged", func(t *testing.T) {
		got := seriesLabel("fixed label", "LogicalDisk", "% Free Space", "C:", "server01")
		if got != "fixed label" {
			t.Errorf("got %q, want %q", got, "fixed label")
		}
	})

	t.Run("legendFormat referencing a macro for missing data substitutes empty string", func(t *testing.T) {
		got := seriesLabel("{{object}} [{{instance}}]", "Memory", "Available Bytes", "", "server01")
		want := "Memory []"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
