package scom

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

type Aggregation string

const (
	AggregationRaw    Aggregation = "raw"
	AggregationHourly Aggregation = "hourly"
	AggregationDaily  Aggregation = "daily"
)

// aggregationTable/valueColumn map an Aggregation to its DW perf view and the
// value column to plot: raw samples carry a single SampleValue, hourly/daily
// rollups carry Min/Max/Avg/StdDev — AverageValue is the natural default for
// a time series panel.
func aggregationTable(agg Aggregation) (table, valueColumn string, err error) {
	switch agg {
	case AggregationRaw, "":
		return "Perf.vPerfRaw", "SampleValue", nil
	case AggregationHourly:
		return "Perf.vPerfHourly", "AverageValue", nil
	case AggregationDaily:
		return "Perf.vPerfDaily", "AverageValue", nil
	default:
		return "", "", fmt.Errorf("unsupported aggregation: %s", agg)
	}
}

// performanceEntityScopeTempTable is the temp table buildPerformanceQuery
// populates with entityIDs when scoping to a class/group selection. See
// idScopeTempTable: a large id list is joined via this table rather than
// folded into a literal IN list, since this query already joins
// Perf.vPerfHourly/Daily/Raw (a huge DW fact view) to three other tables —
// exactly the shape that can make SQL Server's optimizer give up with "the
// query processor ran out of internal resources" once entityIDs is large
// (e.g. a hosting-expanded class/group with hundreds of members).
const performanceEntityScopeTempTable = "#EntityScope"

// buildPerformanceQuery builds the query+args for QueryPerformance. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see performance_test.go.
//
// counterIDs are dbo.PerformanceRuleInstance.PerformanceRuleInstanceRowId
// values, but that table is keyed on (RuleRowId, InstanceName) alone — it has
// no ManagedEntity column, so the *same* PerformanceRuleInstanceRowId is
// shared by every managed entity that reports that rule with that instance
// name (e.g. every computer's "LogicalDisk" counter for instance "C:", or
// every process host's "_Total" instance). The entity a given sample belongs
// to only exists on the fact row itself (Perf.vPerfRaw/Hourly/Daily.
// ManagedEntityRowId). So counterIDs alone never identifies a single class
// instance — entityIDs (already hosting-expanded managed entity guids, or
// nil/empty for "every entity") is required to scope results to the
// instances actually selected, and the caller must also split output series
// per entity, not just per counter — see QueryPerformance below.
func buildPerformanceQuery(counterIDs []string, entityIDs []string, agg Aggregation, from, to time.Time) (string, []any, error) {
	table, valueColumn, err := aggregationTable(agg)
	if err != nil {
		return "", nil, err
	}

	inSQL, inArgs := inClause("ctr", counterIDs)
	args := append([]any{
		sql.Named("from", from),
		sql.Named("to", to),
	}, inArgs...)

	setupSQL := ""
	entityFilter := ""
	if len(entityIDs) > 0 {
		entitySetupSQL, entArgs := idScopeTempTable(performanceEntityScopeTempTable, entityIDs)
		setupSQL = entitySetupSQL
		entityFilter = fmt.Sprintf("\n\tAND me.ManagedEntityGuid IN (SELECT Id FROM %s)", performanceEntityScopeTempTable)
		args = append(args, entArgs...)
	}

	query := setupSQL + fmt.Sprintf(`
SELECT
	p.PerformanceRuleInstanceRowId,
	CONVERT(varchar(64), me.ManagedEntityGuid) AS ManagedEntityGuid,
	pr.ObjectName,
	pr.CounterName,
	pri.InstanceName,
	me.ManagedEntityDefaultName,
	p.DateTime,
	p.%s AS Value
FROM %s p
INNER JOIN dbo.vPerformanceRuleInstance pri ON p.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
INNER JOIN dbo.vManagedEntity me ON p.ManagedEntityRowId = me.ManagedEntityRowId
WHERE p.PerformanceRuleInstanceRowId IN %s
	AND p.DateTime >= @from AND p.DateTime <= @to%s
ORDER BY p.PerformanceRuleInstanceRowId, me.ManagedEntityGuid, p.DateTime`, valueColumn, table, inSQL, entityFilter)

	return query, args, nil
}

// seriesLabelMacros lists the placeholders seriesLabel substitutes in a
// user-supplied legendFormat. Kept alongside seriesLabel so the query
// editor's help text (src/components/CounterPicker.tsx) has one place to
// stay in sync with.
const seriesLabelMacros = "{{object}}, {{counter}}, {{instance}}, {{entity}}"

// seriesLabel renders a performance series' legend. The built-in default —
// "Object - Counter (Entity)", or "Object - Counter [Instance] (Entity)"
// when the counter has a named instance — reads every field there is,
// which gets unwieldy fast (e.g. a LogicalDisk counter against a
// long FQDN: "LogicalDisk - % Free Space [C:] (server01.contoso.example.com)").
// legendFormat lets a query override that with its own template using the
// macros in seriesLabelMacros; instanceName substitutes as "" for a counter
// with no instance, same as it's omitted from the default format.
func seriesLabel(legendFormat, objectName, counterName, instanceName, entityName string) string {
	if legendFormat == "" {
		if instanceName != "" {
			return fmt.Sprintf("%s - %s [%s] (%s)", objectName, counterName, instanceName, entityName)
		}
		return fmt.Sprintf("%s - %s (%s)", objectName, counterName, entityName)
	}

	replacer := strings.NewReplacer(
		"{{object}}", objectName,
		"{{counter}}", counterName,
		"{{instance}}", instanceName,
		"{{entity}}", entityName,
	)
	return replacer.Replace(legendFormat)
}

// QueryPerformance returns one time-series frame per (counter, managed
// entity) pair — counterIDs (from the /counters resource picker) alone don't
// identify a single class instance, since dbo.PerformanceRuleInstance is
// shared across every entity reporting the same rule+instance name; see
// buildPerformanceQuery. entityIDs optionally scopes results to a specific
// set of (already hosting-expanded) managed entities; pass nil/empty for
// "every entity reporting these counters." legendFormat customizes the
// series label — see seriesLabel; pass "" for the built-in default.
func QueryPerformance(ctx context.Context, db *sql.DB, counterIDs []string, entityIDs []string, agg Aggregation, legendFormat string, from, to time.Time) ([]*data.Frame, error) {
	if len(counterIDs) == 0 {
		return nil, nil
	}

	query, args, err := buildPerformanceQuery(counterIDs, entityIDs, agg, from, to)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type seriesKey struct {
		ruleInstanceID  string
		managedEntityID string
	}
	type series struct {
		label  string
		times  []time.Time
		values []float64
	}
	seriesByKey := map[seriesKey]*series{}
	var order []seriesKey

	for rows.Next() {
		var ruleInstanceID, managedEntityID, objectName, counterName, entityName string
		var instanceName sql.NullString
		var ts time.Time
		var value float64

		if err := rows.Scan(&ruleInstanceID, &managedEntityID, &objectName, &counterName, &instanceName, &entityName, &ts, &value); err != nil {
			return nil, err
		}

		// A PerformanceRuleInstanceRowId alone does not identify a single
		// class instance (see buildPerformanceQuery) — the managed entity
		// must be part of the series key too, or samples from every entity
		// sharing this counter+instance-name collapse into one series.
		key := seriesKey{ruleInstanceID: ruleInstanceID, managedEntityID: managedEntityID}
		s, ok := seriesByKey[key]
		if !ok {
			label := seriesLabel(legendFormat, objectName, counterName, instanceName.String, entityName)
			s = &series{label: label}
			seriesByKey[key] = s
			order = append(order, key)
		}
		s.times = append(s.times, ts)
		s.values = append(s.values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frames := make([]*data.Frame, 0, len(order))
	for _, key := range order {
		s := seriesByKey[key]
		valueField := data.NewField("value", nil, s.values)
		// The field name alone ("value") is what Grafana's time series panel
		// uses for the legend, not the frame name — without this, every
		// series in a multi-instance query (e.g. Process\Working Set across
		// every process) shows up as the same indistinguishable "value".
		valueField.Config = &data.FieldConfig{DisplayNameFromDS: s.label}
		frame := data.NewFrame(s.label,
			data.NewField("time", nil, s.times),
			valueField,
		)
		frames = append(frames, frame)
	}
	return frames, nil
}
