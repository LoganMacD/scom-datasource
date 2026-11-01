package scom

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// AggregationAuto picks one stored Data Warehouse grain (raw, hourly or
// daily) per query from the requested range, the panel's point budget and
// the warehouse's live retention settings — see choosePerfGrain. It is never
// passed to aggregationTable; it is always resolved to a concrete grain first.
const AggregationAuto Aggregation = "auto"

// PointBudget is what Grafana says the panel can use: backend.DataQuery's
// MaxDataPoints and Interval, passed through from QueryData. Zero values mean
// "not provided".
type PointBudget struct {
	MaxDataPoints int64
	Interval      time.Duration
}

// target returns the coarsest bucket width the panel can usefully display:
// max(interval, range / maxDataPoints). Without MaxDataPoints it is just the
// interval (possibly zero).
func (b PointBudget) target(from, to time.Time) time.Duration {
	target := b.Interval
	if b.MaxDataPoints > 0 {
		if perPoint := to.Sub(from) / time.Duration(b.MaxDataPoints); perPoint > target {
			target = perPoint
		}
	}
	return target
}

// rawNominalSampleInterval stands in for raw data's width when estimating
// point counts. Raw rows have no fixed width (each collection rule has its
// own interval) and the warehouse metadata has no interval for raw, so this
// is the common SCOM collection interval. It is only used for the point
// budget, never for retention coverage.
const rawNominalSampleInterval = 5 * time.Minute

// perfGrainOrder lists the stored grains from finest to coarsest.
var perfGrainOrder = []Aggregation{AggregationRaw, AggregationHourly, AggregationDaily}

// perfBaseTableNames maps each grain to its dbo.StandardDatasetAggregationStorage.BaseTableName.
// Perf.vPerfRaw/vPerfHourly/vPerfDaily sit over physical tables named
// <BaseTableName>_<guid>, so the base name identifies a grain without
// relying on AggregationTypeId values, which the schema doesn't seed.
var perfBaseTableNames = map[string]Aggregation{
	"perfraw":    AggregationRaw,
	"perfhourly": AggregationHourly,
	"perfdaily":  AggregationDaily,
}

// perfRetention is one grain's retention settings, read from the warehouse.
type perfRetention struct {
	MaxDataAgeDays  int
	IntervalMinutes int // AggregationIntervalDurationMinutes; 0 when NULL
	LastGrooming    sql.NullTime
}

// perfRetentionQuery reads retention for the Perf dataset's grains. The
// dataset is found by its schema name ('Perf', the schema of the vPerf*
// views) and each grain by its storage base table name — see
// perfBaseTableNames.
const perfRetentionQuery = `
SELECT
	sdas.BaseTableName,
	sda.MaxDataAgeDays,
	sda.AggregationIntervalDurationMinutes,
	sda.LastGroomingDateTime
FROM dbo.StandardDataset sd
INNER JOIN dbo.StandardDatasetAggregation sda ON sda.DatasetId = sd.DatasetId
INNER JOIN dbo.StandardDatasetAggregationStorage sdas ON sdas.DatasetId = sda.DatasetId
	AND sdas.AggregationTypeId = sda.AggregationTypeId
WHERE sd.SchemaName = 'Perf'`

// loadPerfRetention returns the retention settings for each grain it can
// identify. A grain missing from the result is skipped by choosePerfGrain,
// never invented.
func loadPerfRetention(ctx context.Context, db *sql.DB) (map[Aggregation]perfRetention, error) {
	rows, err := db.QueryContext(ctx, perfRetentionQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[Aggregation]perfRetention{}
	for rows.Next() {
		var baseTable string
		var maxAge int
		var interval sql.NullInt64
		var lastGroom sql.NullTime
		if err := rows.Scan(&baseTable, &maxAge, &interval, &lastGroom); err != nil {
			return nil, err
		}
		agg, ok := perfBaseTableNames[normalizeBaseTableName(baseTable)]
		if !ok {
			continue
		}
		out[agg] = perfRetention{MaxDataAgeDays: maxAge, IntervalMinutes: int(interval.Int64), LastGrooming: lastGroom}
	}
	return out, rows.Err()
}

// normalizeBaseTableName lowercases a base table name and drops any schema
// prefix or brackets ("[Perf].[PerfRaw]" -> "perfraw").
func normalizeBaseTableName(name string) string {
	name = strings.NewReplacer("[", "", "]", "").Replace(name)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// grainWidth is a grain's native width for point-budget purposes.
func grainWidth(agg Aggregation, r perfRetention) time.Duration {
	if r.IntervalMinutes > 0 && agg != AggregationRaw {
		return time.Duration(r.IntervalMinutes) * time.Minute
	}
	switch agg {
	case AggregationHourly:
		return time.Hour
	case AggregationDaily:
		return 24 * time.Hour
	default:
		return rawNominalSampleInterval
	}
}

// retainedSince is the oldest time a grain is expected to still hold data,
// relative to now (not to the query's end): grooming deletes rows older than
// MaxDataAgeDays.
func retainedSince(r perfRetention, now time.Time) time.Time {
	return now.AddDate(0, 0, -r.MaxDataAgeDays)
}

func grainLabel(agg Aggregation) string {
	switch agg {
	case AggregationHourly:
		return "hourly"
	case AggregationDaily:
		return "daily"
	default:
		return "raw"
	}
}

// perfGrainChoice is the grain a performance query will read, plus what the
// user should be told about it and the inputs behind it (for logging).
type perfGrainChoice struct {
	Aggregation Aggregation // always raw, hourly or daily
	Notices     []data.Notice
	Reason      string
	Target      time.Duration
}

// choosePerfGrain resolves the query's saved aggregation to the one grain it
// will read.
//
// Explicit raw/hourly/daily (and an unset aggregation, which has always meant
// raw) are never overridden. If the chosen grain no longer covers the
// requested start, a warning says so, but the grain stays the same.
//
// Auto picks, among grains whose retention reaches back to from, the finest
// one whose width is at least the panel's target bucket (so the estimated
// point count fits MaxDataPoints). If the target is finer than every
// covering grain, that is the finest covering grain; if it is coarser than
// every covering grain, the coarsest. Without MaxDataPoints there is no
// budget and the finest covering grain wins. If no grain covers from, the
// longest-retained grain is used with a warning. If retention can't be read
// at all, Auto falls back to hourly for targets up to a day and daily
// beyond — not raw, which is the grain that silently truncates.
//
// Grains are never combined: raw SampleValue and hourly/daily AverageValue
// are different statistics, and the bucket boundaries needed to stitch
// them safely aren't established.
func choosePerfGrain(requested Aggregation, retention map[Aggregation]perfRetention, retentionErr error, from, to, now time.Time, budget PointBudget) perfGrainChoice {
	target := budget.target(from, to)

	if requested != AggregationAuto {
		agg := requested
		if agg == "" {
			agg = AggregationRaw
		}
		choice := perfGrainChoice{Aggregation: agg, Reason: "explicit", Target: target}
		if r, ok := retention[agg]; ok && retentionErr == nil {
			if since := retainedSince(r, now); from.Before(since) {
				hint := " Choose Auto or a coarser aggregation to see the whole range."
				if agg == AggregationDaily {
					hint = ""
				}
				choice.Notices = append(choice.Notices, data.Notice{
					Severity: data.NoticeSeverityWarning,
					Text: fmt.Sprintf("The time range starts before the oldest retained %s performance data (kept %d days, so about %s onwards). Only retained data is shown.%s",
						grainLabel(agg), r.MaxDataAgeDays, since.UTC().Format("2006-01-02 15:04 MST"), hint),
				})
				choice.Reason = "explicit, start predates retention"
			}
		}
		return choice
	}

	if retentionErr != nil || len(retention) == 0 {
		agg := AggregationHourly
		if target > 24*time.Hour {
			agg = AggregationDaily
		}
		return perfGrainChoice{
			Aggregation: agg,
			Reason:      "auto, retention metadata unavailable",
			Target:      target,
			Notices: []data.Notice{{
				Severity: data.NoticeSeverityWarning,
				Text:     fmt.Sprintf("Auto aggregation couldn't read the Data Warehouse retention settings, so it used %s data. Check that the warehouse account can read dbo.StandardDataset, dbo.StandardDatasetAggregation and dbo.StandardDatasetAggregationStorage.", grainLabel(agg)),
			}},
		}
	}

	var covering []Aggregation
	for _, agg := range perfGrainOrder {
		r, ok := retention[agg]
		if ok && !from.Before(retainedSince(r, now)) {
			covering = append(covering, agg)
		}
	}

	if len(covering) == 0 {
		// Ties go to the coarser grain: same reach, far fewer rows over a range
		// this long (hourly and daily often share one retention setting).
		var best Aggregation
		for _, agg := range perfGrainOrder {
			r, ok := retention[agg]
			if ok && (best == "" || r.MaxDataAgeDays >= retention[best].MaxDataAgeDays) {
				best = agg
			}
		}
		r := retention[best]
		return perfGrainChoice{
			Aggregation: best,
			Reason:      "auto, no grain covers start",
			Target:      target,
			Notices: []data.Notice{{
				Severity: data.NoticeSeverityWarning,
				Text: fmt.Sprintf("The time range starts before any retained performance data. Showing %s data, the longest kept (%d days, so about %s onwards).",
					grainLabel(best), r.MaxDataAgeDays, retainedSince(r, now).UTC().Format("2006-01-02 15:04 MST")),
			}},
		}
	}

	chosen := covering[0]
	reason := "auto, finest covering grain (no point budget)"
	if budget.MaxDataPoints > 0 {
		chosen = covering[len(covering)-1]
		reason = "auto, target coarser than every covering grain"
		for _, agg := range covering {
			if grainWidth(agg, retention[agg]) >= target {
				chosen = agg
				reason = "auto, finest covering grain within point budget"
				break
			}
		}
	}

	return perfGrainChoice{
		Aggregation: chosen,
		Reason:      reason,
		Target:      target,
		Notices: []data.Notice{{
			Severity: data.NoticeSeverityInfo,
			Text:     fmt.Sprintf("Auto aggregation is showing %s data for this time range.", grainLabel(chosen)),
		}},
	}
}
