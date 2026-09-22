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
//
// When counterIDs is empty, the counter is selected by object/counterName
// instead — "every instance reporting this counter." That is joined by name
// against dbo.vPerformanceRule (already in the query) rather than first
// resolving thousands of PerformanceRuleInstanceRowIds into a literal IN
// list, which is what used to blow the optimizer's resource limit (and the
// ~2,100 RPC parameter cap) on broad selections.
func buildPerformanceQuery(counterIDs []string, object, counterName string, entityIDs []string, agg Aggregation, from, to time.Time) (string, []any, error) {
	table, valueColumn, err := aggregationTable(agg)
	if err != nil {
		return "", nil, err
	}

	args := []any{
		sql.Named("from", from),
		sql.Named("to", to),
	}

	var counterFilter string
	if len(counterIDs) > 0 {
		inSQL, inArgs := inClause("ctr", counterIDs)
		counterFilter = "p.PerformanceRuleInstanceRowId IN " + inSQL
		args = append(args, inArgs...)
	} else {
		counterFilter = "pr.ObjectName = @object AND pr.CounterName = @counterName"
		args = append(args, sql.Named("object", object), sql.Named("counterName", counterName))
	}

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
	tme.ManagedEntityDefaultName AS HostDisplayName,
	p.DateTime,
	p.%s AS Value
FROM %s p
INNER JOIN dbo.vPerformanceRuleInstance pri ON p.PerformanceRuleInstanceRowId = pri.PerformanceRuleInstanceRowId
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
INNER JOIN dbo.vManagedEntity me ON p.ManagedEntityRowId = me.ManagedEntityRowId
LEFT JOIN dbo.vManagedEntity tme ON tme.ManagedEntityRowId = COALESCE(me.TopLevelHostManagedEntityRowId, me.ManagedEntityRowId)
WHERE %s
	AND p.DateTime >= @from AND p.DateTime <= @to%s
ORDER BY p.PerformanceRuleInstanceRowId, me.ManagedEntityGuid, p.DateTime`, valueColumn, table, counterFilter, entityFilter)

	return query, args, nil
}

// seriesLabel renders a performance series' legend. The built-in default —
// "Object - Counter (Entity)", or "Object - Counter [Instance] (Entity)"
// when the counter has a named instance — reads every field there is,
// which gets unwieldy fast (e.g. a LogicalDisk counter against a
// long FQDN: "LogicalDisk - % Free Space [C:] (server01.contoso.example.com)").
// legendFormat lets a query override that with its own template using the
// {{object}}, {{counter}}, {{instance}}, {{entity}}, {{host}} macros (kept in
// sync with the query editor's help text in
// src/components/CounterPicker.tsx); instanceName substitutes as "" for a
// counter with no instance, same as it's omitted from the default format.
// entityName is the managed entity the counter was collected against (e.g.
// a "Processor Information" object instance), which for a hosted object is
// often not the computer itself — hostName is that object's top-level
// hosting entity's display name (typically the computer), falling back to
// entityName when the entity has no distinct host (it is itself top-level).
func seriesLabel(legendFormat, objectName, counterName, instanceName, entityName, hostName string) string {
	if hostName == "" {
		hostName = entityName
	}

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
		"{{host}}", hostName,
	)
	return replacer.Replace(legendFormat)
}

// seriesLabels builds the dimensional labels carried on a performance series'
// value field. seriesLabel flattens these same facts into one rendered string
// for the legend, which is display only — nothing downstream can take it
// apart again. Labels are what Grafana's dimensional transformations read
// ("Labels to fields", "Prepare time series", "Partition by values"), so
// without them a dashboard has no way to filter or group by instance/entity:
// the instance name only exists as a substring of a sentence. Names are kept
// in sync with the legendFormat macros ({{object}} etc., see seriesLabel) so
// a user who knows one knows the other.
//
// counterID/entityID are the raw ids making up QueryPerformance's series key,
// and they are labels rather than an implementation detail on purpose: the
// display names alone are not unique. ManagedEntityDefaultName is a computed
// alias of dbo.ManagedEntity.DisplayName with no uniqueness constraint (two
// re-imaged or cloned agents routinely share one), and two rules from
// different management packs can carry the same ObjectName+CounterName, so
// (object, counter, instance, entity, host) can repeat across two genuinely
// distinct series. Grafana's timeseries-multi contract requires each frame's
// label set to identify it uniquely — duplicates get collapsed or collide
// once a transformation groups on them. Including the series key itself makes
// that uniqueness true by construction rather than by luck.
//
// They are deliberately *not* among the columns performanceDimensionFields
// emits, which is the whole division of labour here: labels are identity, and
// have to be unique even when that costs readability; columns are what a
// person filters and groups on, and two guid columns in a table are noise.
// That split also keeps the one real cost of carrying them contained —
// pivoting labels into columns ("Labels to fields", "Prepare time series →
// Long") turns each unique-per-series guid into a column of its own and
// leaves the follow-up merge nothing to join on. Since the dimensions already
// arrive as columns, that pivot isn't something a dashboard needs to reach
// for any more.
//
// instance is always present, empty string and all, even though seriesLabel
// drops an empty instance from the rendered legend: a response mixing
// instanced and non-instanced counters (a multi-counter counterIDs selection)
// would otherwise hand Grafana frames with different label *keys*, which
// makes "Labels to fields" produce ragged columns.
func seriesLabels(counterID, entityID, objectName, counterName, instanceName, entityName, hostName string) data.Labels {
	if hostName == "" {
		hostName = entityName
	}
	return data.Labels{
		"object":    objectName,
		"counter":   counterName,
		"instance":  instanceName,
		"entity":    entityName,
		"host":      hostName,
		"counterId": counterID,
		"entityId":  entityID,
	}
}

// PerformanceFormat selects the shape QueryPerformance returns.
type PerformanceFormat string

const (
	// PerformanceFormatTimeSeries (also the zero value, so a query saved
	// before this field existed keeps its shape) returns one frame per
	// series — what the time series panel wants.
	PerformanceFormatTimeSeries PerformanceFormat = "timeseries"
	// PerformanceFormatTable returns a single long frame with one row per
	// sample and the series' dimensions as plain string columns, so a
	// dashboard can filter or group on instance/entity/host with ordinary
	// transformations (Filter data by values, Group by, ...) instead of
	// having to pivot labels out of a value field first.
	PerformanceFormatTable PerformanceFormat = "table"
)

// performanceSeries is one (counter, managed entity) series' samples.
type performanceSeries struct {
	label  string
	labels data.Labels
	times  []time.Time
	values []float64
}

// performanceDimensions is the order the dimension columns are emitted in,
// by both result formats. These are seriesLabels' operator-facing keys, under
// the same names, so a query reads the same whichever format it's in — but
// deliberately not its counterId/entityId, which exist to make a label set
// unique and would only be noise in a table. See seriesLabels.
var performanceDimensions = []string{"object", "counter", "instance", "entity", "host"}

// performanceDimensionFields repeats one series' dimensions down its own
// length, as plain string columns.
func performanceDimensionFields(s *performanceSeries) []*data.Field {
	fields := make([]*data.Field, 0, len(performanceDimensions))
	for _, d := range performanceDimensions {
		column := make([]string, len(s.times))
		for i := range column {
			column[i] = s.labels[d]
		}
		fields = append(fields, data.NewField(d, nil, column))
	}
	return fields
}

// performanceFrames shapes series into frames. Split out from
// QueryPerformance so both formats are directly testable without a live DB —
// see performance_test.go.
func performanceFrames(series []*performanceSeries, format PerformanceFormat) []*data.Frame {
	if format == PerformanceFormatTable {
		return []*data.Frame{performanceTableFrame(series)}
	}

	frames := make([]*data.Frame, 0, len(series))
	for _, s := range series {
		// Labels carry the series' dimensions in a form transformations can
		// still take apart (see seriesLabels); DisplayNameFromDS carries the
		// rendered legend. Both are needed: the field name alone ("value") is
		// what Grafana's time series panel uses for the legend, not the frame
		// name — without DisplayNameFromDS, every series in a multi-instance
		// query (e.g. Process\Working Set across every process) shows up as
		// the same indistinguishable "value", and with labels but no display
		// name it degrades instead to an auto-generated
		// value{object="...", counter="...", ...} dump.
		valueField := data.NewField("value", s.labels, s.values)
		valueField.Config = &data.FieldConfig{DisplayNameFromDS: s.label}
		frame := data.NewFrame(s.label,
			data.NewField("time", nil, s.times),
			valueField,
		)
		// The dimensions again, as plain string columns, so a panel can
		// filter or group on instance/entity/host directly instead of first
		// pivoting them out of the labels above. Appended after time/value
		// rather than between them, so the first two fields stay exactly the
		// [time, value] pair a plain timeseries-multi frame has and anything
		// reading the shape positionally sees no difference. The labels stay
		// put: these columns are additional to them, not a replacement, so
		// an existing ${__field.labels.x} override keeps working.
		//
		// The dataplane spec says a frame needs "at least time and one
		// numeric value column, with the first occurrence of each type being
		// used for the series", which reads as extra string fields being
		// tolerated and ignored when picking the series. That is a reading of
		// the spec rather than something verified against a running Grafana —
		// a consumer that classifies frames by inspecting fields instead of
		// reading Meta.Type could decide these columns make this long format.
		frame.Fields = append(frame.Fields, performanceDimensionFields(s)...)
		// Declaring the dataplane contract rather than leaving Grafana to
		// sniff the shape: one time field and one numeric field each, times
		// ascending per series (the query's ORDER BY ... p.DateTime
		// guarantees it).
		frame.Meta = &data.FrameMeta{
			Type:        data.FrameTypeTimeSeriesMulti,
			TypeVersion: data.FrameTypeVersion{0, 1},
		}
		frames = append(frames, frame)
	}
	return frames
}

// performanceTableFrame flattens every series into one long frame: time, one
// string column per dimension, then value. Rows stay grouped by series, in
// the order the query returned them, rather than re-sorted by time. No
// dataplane Type is declared — the rows aren't globally time-ordered, which
// timeseries-long expects — so Grafana treats it as the plain table it is.
func performanceTableFrame(series []*performanceSeries) *data.Frame {
	n := 0
	for _, s := range series {
		n += len(s.times)
	}

	times := make([]time.Time, 0, n)
	values := make([]float64, 0, n)
	dims := make(map[string][]string, len(performanceDimensions))
	for _, d := range performanceDimensions {
		dims[d] = make([]string, 0, n)
	}

	for _, s := range series {
		times = append(times, s.times...)
		values = append(values, s.values...)
		for _, d := range performanceDimensions {
			v := s.labels[d]
			for range s.times {
				dims[d] = append(dims[d], v)
			}
		}
	}

	fields := make([]*data.Field, 0, len(performanceDimensions)+2)
	fields = append(fields, data.NewField("time", nil, times))
	for _, d := range performanceDimensions {
		fields = append(fields, data.NewField(d, nil, dims[d]))
	}
	fields = append(fields, data.NewField("value", nil, values))

	return data.NewFrame("performance", fields...).SetMeta(&data.FrameMeta{PreferredVisualization: data.VisTypeTable})
}

// QueryPerformance reads one series per (counter, managed entity) pair —
// counterIDs (from the /counters resource picker) alone don't identify a
// single class instance, since dbo.PerformanceRuleInstance is shared across
// every entity reporting the same rule+instance name; see
// buildPerformanceQuery. entityIDs optionally scopes results to a specific
// set of (already hosting-expanded) managed entities; pass nil/empty for
// "every entity reporting these counters." legendFormat customizes the
// series label — see seriesLabel; pass "" for the built-in default.
//
// format picks the result's shape — see performanceFrames. The default is
// Grafana's timeseries-multi contract: one frame per series, each with a time
// field and a "value" field whose labels hold the series' dimensions (see
// seriesLabels) and whose DisplayNameFromDS holds the rendered legend.
// Multi-frame rather than wide because SCOM agents submit on their own
// cadence — timestamps rarely align across entities, so a shared time column
// would be mostly nulls, and increasingly so the more entities are in scope.
func QueryPerformance(ctx context.Context, db *sql.DB, counterIDs []string, object, counterName string, entityIDs []string, agg Aggregation, legendFormat string, format PerformanceFormat, from, to time.Time) ([]*data.Frame, error) {
	if len(counterIDs) == 0 && (object == "" || counterName == "") {
		return nil, nil
	}

	query, args, err := buildPerformanceQuery(counterIDs, object, counterName, entityIDs, agg, from, to)
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
	seriesByKey := map[seriesKey]*performanceSeries{}
	var order []*performanceSeries

	for rows.Next() {
		var ruleInstanceID, managedEntityID, objectName, counterName, entityName string
		var instanceName, hostName sql.NullString
		var ts time.Time
		var value float64

		if err := rows.Scan(&ruleInstanceID, &managedEntityID, &objectName, &counterName, &instanceName, &entityName, &hostName, &ts, &value); err != nil {
			return nil, err
		}

		// A PerformanceRuleInstanceRowId alone does not identify a single
		// class instance (see buildPerformanceQuery) — the managed entity
		// must be part of the series key too, or samples from every entity
		// sharing this counter+instance-name collapse into one series.
		key := seriesKey{ruleInstanceID: ruleInstanceID, managedEntityID: managedEntityID}
		s, ok := seriesByKey[key]
		if !ok {
			label := seriesLabel(legendFormat, objectName, counterName, instanceName.String, entityName, hostName.String)
			labels := seriesLabels(ruleInstanceID, managedEntityID, objectName, counterName, instanceName.String, entityName, hostName.String)
			s = &performanceSeries{label: label, labels: labels}
			seriesByKey[key] = s
			order = append(order, s)
		}
		s.times = append(s.times, ts)
		s.values = append(s.values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return performanceFrames(order, format), nil
}
