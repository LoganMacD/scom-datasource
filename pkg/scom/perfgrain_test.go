package scom

import (
	"errors"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

func TestChoosePerfGrain(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	standard := map[Aggregation]perfRetention{
		AggregationRaw:    {MaxDataAgeDays: 10},
		AggregationHourly: {MaxDataAgeDays: 400, IntervalMinutes: 60},
		AggregationDaily:  {MaxDataAgeDays: 400, IntervalMinutes: 1440},
	}
	panel := PointBudget{MaxDataPoints: 1000, Interval: 20 * time.Second}

	cases := []struct {
		name         string
		requested    Aggregation
		retention    map[Aggregation]perfRetention
		retentionErr error
		from, to     time.Time
		budget       PointBudget
		want         Aggregation
		wantSeverity []data.NoticeSeverity
	}{
		{"auto short recent range stays raw", AggregationAuto, standard, nil, now.Add(-6 * time.Hour), now, panel, AggregationRaw, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto 90 days over hourly budget goes daily", AggregationAuto, standard, nil, now.Add(-90 * day), now, panel, AggregationDaily, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto 90 days with a larger budget stays hourly", AggregationAuto, standard, nil, now.Add(-90 * day), now, PointBudget{MaxDataPoints: 3000}, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto without point budget uses finest covering grain", AggregationAuto, standard, nil, now.Add(-90 * day), now, PointBudget{}, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto short range that ended in the past skips raw", AggregationAuto, standard, nil, now.Add(-30*day - 6*time.Hour), now.Add(-30 * day), panel, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto 7 days exceeds raw budget", AggregationAuto, standard, nil, now.Add(-7 * day), now, panel, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto target finer than every covering grain picks finest", AggregationAuto, standard, nil, now.Add(-2 * day), now, PointBudget{MaxDataPoints: 100000}, AggregationRaw, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto target coarser than every covering grain picks coarsest", AggregationAuto, standard, nil, now.Add(-300 * day), now, PointBudget{MaxDataPoints: 10}, AggregationDaily, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto nothing covers start uses longest retained, coarser on tie", AggregationAuto, standard, nil, now.Add(-3 * 365 * day), now, panel, AggregationDaily, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"auto missing hourly metadata is skipped", AggregationAuto, map[Aggregation]perfRetention{
			AggregationRaw: {MaxDataAgeDays: 10}, AggregationDaily: {MaxDataAgeDays: 400},
		}, nil, now.Add(-30 * day), now, panel, AggregationDaily, []data.NoticeSeverity{data.NoticeSeverityInfo}},
		{"auto retention error, target up to a day, falls back to hourly", AggregationAuto, nil, errors.New("permission denied"), now.Add(-90 * day), now, panel, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"auto retention error, target over a day, falls back to daily", AggregationAuto, nil, errors.New("permission denied"), now.Add(-3 * 365 * day), now, PointBudget{MaxDataPoints: 100}, AggregationDaily, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"auto empty retention is treated as unavailable", AggregationAuto, map[Aggregation]perfRetention{}, nil, now.Add(-6 * time.Hour), now, panel, AggregationHourly, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"explicit raw past retention stays raw with warning", AggregationRaw, standard, nil, now.Add(-90 * day), now, panel, AggregationRaw, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"unset aggregation still means raw", "", standard, nil, now.Add(-90 * day), now, panel, AggregationRaw, []data.NoticeSeverity{data.NoticeSeverityWarning}},
		{"explicit raw within retention has no notice", AggregationRaw, standard, nil, now.Add(-6 * time.Hour), now, panel, AggregationRaw, nil},
		{"explicit hourly is never overridden", AggregationHourly, standard, nil, now.Add(-6 * time.Hour), now, panel, AggregationHourly, nil},
		{"explicit raw with retention error runs unchanged", AggregationRaw, nil, errors.New("permission denied"), now.Add(-90 * day), now, panel, AggregationRaw, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := choosePerfGrain(c.requested, c.retention, c.retentionErr, c.from, c.to, now, c.budget)
			if got.Aggregation != c.want {
				t.Errorf("chose %q (%s), want %q", got.Aggregation, got.Reason, c.want)
			}
			if len(got.Notices) != len(c.wantSeverity) {
				t.Fatalf("got %d notices %+v, want %d", len(got.Notices), got.Notices, len(c.wantSeverity))
			}
			for i, n := range got.Notices {
				if n.Severity != c.wantSeverity[i] {
					t.Errorf("notice %d severity = %v, want %v", i, n.Severity, c.wantSeverity[i])
				}
			}
		})
	}
}

func TestNormalizeBaseTableName(t *testing.T) {
	for in, want := range map[string]string{
		"PerfRaw":             "perfraw",
		"[Perf].[PerfHourly]": "perfhourly",
		" PerfDaily ":         "perfdaily",
	} {
		if got := normalizeBaseTableName(in); got != want {
			t.Errorf("normalizeBaseTableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregationAutoIsNotAView(t *testing.T) {
	if _, _, err := aggregationTable(AggregationAuto); err == nil {
		t.Error("aggregationTable(auto) should error: auto must be resolved by choosePerfGrain first")
	}
}
