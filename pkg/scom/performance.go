package scom

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

type Aggregation string

const (
	AggregationRaw    Aggregation = "raw"
	AggregationHourly Aggregation = "hourly"
	AggregationDaily  Aggregation = "daily"
)

// Statistic picks which of an hourly/daily rollup's statistics a performance
// query plots. Unset (including on a query saved before this field existed)
// means the average, which is all those grains ever showed. Raw rows are
// single samples, so every statistic of a raw row is the sample itself.
type Statistic string

const (
	StatisticAverage Statistic = "average"
	StatisticMin     Statistic = "min"
	StatisticMax     Statistic = "max"
)

func validStatistic(stat Statistic) bool {
	switch stat {
	case "", StatisticAverage, StatisticMin, StatisticMax:
		return true
	}
	return false
}

// aggregationTable maps an Aggregation to its DW perf view and default value
// column: raw samples carry a single SampleValue, hourly/daily rollups carry
// Min/Max/Avg/StdDev, and AverageValue is the default for a time series.
func aggregationTable(agg Aggregation) (table, valueColumn string, err error) {
	switch agg {
	case AggregationRaw, "":
		return "Perf.vPerfRaw", "SampleValue", nil
	case AggregationHourly:
		return "Perf.vPerfHourly", "AverageValue", nil
	case AggregationDaily:
		return "Perf.vPerfDaily", "AverageValue", nil
	default:
		return "", "", badRequestf("unsupported aggregation: %s", agg)
	}
}

// perfValueColumn is the column a query plots for a grain and statistic.
func perfValueColumn(agg Aggregation, stat Statistic) (string, error) {
	_, column, err := aggregationTable(agg)
	if err != nil {
		return "", err
	}
	if column == "SampleValue" {
		return column, nil
	}
	switch stat {
	case "", StatisticAverage:
		return "AverageValue", nil
	case StatisticMin:
		return "MinValue", nil
	case StatisticMax:
		return "MaxValue", nil
	default:
		return "", badRequestf("unsupported statistic: %s", stat)
	}
}

// Temp tables buildPerformanceQuery uses. Connection-scoped, so the fixed
// names are safe across concurrent requests.
const (
	// performanceEntityScopeTempTable holds the scope's managed entity guids
	// (see idScopeTempTable: a large id list goes through a temp table, never
	// a literal IN list).
	performanceEntityScopeTempTable = "#EntityScope"
	// performanceEntityRowsTempTable holds the same entities as DW
	// ManagedEntityRowIds, so the fact view is filtered on its own column
	// instead of being joined to dbo.vManagedEntity row by row.
	performanceEntityRowsTempTable = "#EntityRows"
	// performanceCounterRowsTempTable holds the PerformanceRuleInstanceRowIds
	// of an object/counter chosen by name.
	performanceCounterRowsTempTable = "#CounterRows"
)

// buildPerformanceQuery builds the data query for QueryPerformance. Kept
// separate from execution so the generated SQL can be asserted against in
// tests without a live DB — see performance_test.go.
//
// The query reads only ids, the timestamp and the value from the fact view;
// names are looked up once per series afterwards (see
// buildPerformanceSeriesQuery). Repeating six strings on every row made the
// rows roughly ten times larger than the numbers they carry.
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
// instead — "every instance reporting this counter." Those names are
// resolved to rule instance ids in a temp table rather than into a literal
// IN list, which is what used to blow the optimizer's resource limit (and
// the ~2,100 RPC parameter cap) on broad selections.
//
// There's no ORDER BY: sorting every row on the server needs a large memory
// grant, and QueryPerformance sorts each series itself.
func buildPerformanceQuery(counterIDs []int64, object, counterName string, entityIDs []string, agg Aggregation, stat Statistic, from, to time.Time) (string, []any, error) {
	table, _, err := aggregationTable(agg)
	if err != nil {
		return "", nil, err
	}
	column, err := perfValueColumn(agg, stat)
	if err != nil {
		return "", nil, err
	}

	args := []any{
		sql.Named("from", from),
		sql.Named("to", to),
	}
	var setup strings.Builder
	var where []string

	if len(counterIDs) > 0 {
		inSQL, inArgs := int64InClause("ctr", counterIDs)
		where = append(where, "p.PerformanceRuleInstanceRowId IN "+inSQL)
		args = append(args, inArgs...)
	} else {
		fmt.Fprintf(&setup, `
CREATE TABLE %[1]s (Id int PRIMARY KEY);
INSERT INTO %[1]s (Id)
SELECT DISTINCT pri.PerformanceRuleInstanceRowId
FROM dbo.vPerformanceRuleInstance pri
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
WHERE pr.ObjectName = @object AND pr.CounterName = @counterName;
`, performanceCounterRowsTempTable)
		where = append(where, fmt.Sprintf("p.PerformanceRuleInstanceRowId IN (SELECT Id FROM %s)", performanceCounterRowsTempTable))
		args = append(args, sql.Named("object", object), sql.Named("counterName", counterName))
	}

	where = append(where, "p.DateTime >= @from AND p.DateTime <= @to")

	if len(entityIDs) > 0 {
		scopeSQL, scopeArgs := idScopeTempTable(performanceEntityScopeTempTable, entityIDs)
		setup.WriteString(scopeSQL)
		fmt.Fprintf(&setup, `
CREATE TABLE %[1]s (Id int PRIMARY KEY);
INSERT INTO %[1]s (Id)
SELECT DISTINCT me.ManagedEntityRowId
FROM dbo.vManagedEntity me
INNER JOIN %[2]s s ON s.Id = me.ManagedEntityGuid;
`, performanceEntityRowsTempTable, performanceEntityScopeTempTable)
		where = append(where, fmt.Sprintf("p.ManagedEntityRowId IN (SELECT Id FROM %s)", performanceEntityRowsTempTable))
		args = append(args, scopeArgs...)
	}

	query := setup.String() + fmt.Sprintf(`
SELECT
	p.PerformanceRuleInstanceRowId,
	p.ManagedEntityRowId,
	p.DateTime,
	p.%s AS Value
FROM %s p
WHERE %s`, column, table, strings.Join(where, "\n\tAND "))

	return query, args, nil
}

// buildPerformanceSeriesQuery looks up the names behind the series a data
// query returned: one result set for the rule instances (object, counter,
// instance) and one for the managed entities (guid, display name, top-level
// host). Both id lists go through temp tables (see intScopeTempTable).
//
// The entity name falls back to its Name, then its guid:
// ManagedEntityDefaultName is a computed alias of the nullable DisplayName.
func buildPerformanceSeriesQuery(counterRowIDs, entityRowIDs []int64) (string, []any) {
	counterSQL, counterArgs := intScopeTempTable("#SeriesCounters", "seriesCounterIds", counterRowIDs)
	entitySQL, entityArgs := intScopeTempTable("#SeriesEntities", "seriesEntityIds", entityRowIDs)
	query := counterSQL + entitySQL + `
SELECT
	pri.PerformanceRuleInstanceRowId,
	pr.ObjectName,
	pr.CounterName,
	pri.InstanceName
FROM dbo.vPerformanceRuleInstance pri
INNER JOIN dbo.vPerformanceRule pr ON pri.RuleRowId = pr.RuleRowId
WHERE pri.PerformanceRuleInstanceRowId IN (SELECT Id FROM #SeriesCounters);

SELECT
	me.ManagedEntityRowId,
	CONVERT(varchar(64), me.ManagedEntityGuid) AS ManagedEntityGuid,
	COALESCE(me.ManagedEntityDefaultName, me.Name, CONVERT(nvarchar(64), me.ManagedEntityGuid)) AS EntityName,
	tme.ManagedEntityDefaultName AS HostDisplayName
FROM dbo.vManagedEntity me
LEFT JOIN dbo.vManagedEntity tme ON tme.ManagedEntityRowId = COALESCE(me.TopLevelHostManagedEntityRowId, me.ManagedEntityRowId)
WHERE me.ManagedEntityRowId IN (SELECT Id FROM #SeriesEntities);`
	return query, append(counterArgs, entityArgs...)
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

// filterableValueField is a second copy of a series' values, carried purely
// so "Filter data by values" has something to aim at. Opt-in per query — see
// PerformanceQuery.FilterableValue.
//
// The graphed value field can't serve that purpose. Grafana resolves a
// field's display name once, in calculateFieldDisplayName, and both the graph
// legend and the transformation read that same result — the transformation
// looks its target field up by display name (see groupFieldIndexByName in
// filterByValue). Since the legend has to differ per series to be any use,
// and a filter needs one name that means the same thing in every frame, one
// field cannot satisfy both. This copy takes the stable name and leaves the
// original to the legend.
//
// Config.DisplayName rather than the field name, because DisplayName is what
// calculateFieldDisplayName returns first — ahead of the branch that would
// otherwise prefix this frame's name (its series' legend) onto it and make
// the column differ per frame all over again.
//
// hideFrom keeps the copy out of the graph. Without it a second numeric field
// draws a second, identical line per series and doubles the legend. It is the
// time series panel's own custom config (HideSeriesConfig in
// @grafana/schema), so panels that don't know it — a table, for instance —
// show the column, which is where you'd want to see it anyway.
func filterableValueField(values []float64) *data.Field {
	field := data.NewField("valueCopy", nil, append([]float64(nil), values...))
	field.Config = &data.FieldConfig{
		DisplayName: "value",
		Custom: map[string]any{
			"hideFrom": map[string]any{"viz": true, "legend": true, "tooltip": true},
		},
	}
	return field
}

// PerformanceQuery is what QueryPerformance runs.
type PerformanceQuery struct {
	// CounterIDs are PerformanceRuleInstanceRowIds picked in the editor. When
	// empty, Object and CounterName select every instance of that counter.
	CounterIDs  []int64
	Object      string
	CounterName string
	// EntityIDs are hosting-expanded managed entity guids; empty means every
	// entity reporting the counters.
	EntityIDs   []string
	Aggregation Aggregation // as saved; see choosePerfGrain
	Statistic   Statistic
	// LegendFormat customizes the series label — see seriesLabel; "" is the
	// built-in default.
	LegendFormat string
	// FilterableValue adds filterableValueField to every frame.
	FilterableValue bool
	From, To        time.Time
	Budget          PointBudget
}

// perfSeriesKey identifies one series: a rule instance on one entity.
type perfSeriesKey struct {
	counterRowID int64
	entityRowID  int64
}

type perfPoint struct {
	t time.Time
	v float64
}

type perfCounterMeta struct {
	object, counter, instance string
}

type perfEntityMeta struct {
	guid, name, host string
}

// QueryPerformance returns one time-series frame per (counter, managed
// entity) pair — counterIDs (from the /counters resource picker) alone don't
// identify a single class instance, since dbo.PerformanceRuleInstance is
// shared across every entity reporting the same rule+instance name; see
// buildPerformanceQuery.
//
// Frames follow Grafana's timeseries-multi contract: each carries a time
// field and a "value" field whose labels hold the series' dimensions (see
// seriesLabels) and whose DisplayNameFromDS holds the rendered legend.
// Multi-frame rather than wide because SCOM agents submit on their own
// cadence — timestamps rarely align across entities, so a shared time column
// would be mostly nulls, and increasingly so the more entities are in scope.
// Frames are sorted by legend, so the order (and the colors Grafana assigns
// by position) stays the same from one refresh to the next.
//
// The saved aggregation is resolved by choosePerfGrain (including Auto) to
// the one stored grain this query reads, using the warehouse's retention
// settings and the panel's point budget. Notices about that choice go on the
// first frame.
func QueryPerformance(ctx context.Context, db *DB, q PerformanceQuery) ([]*data.Frame, error) {
	if len(q.CounterIDs) == 0 && (q.Object == "" || q.CounterName == "") {
		return nil, nil
	}

	logger := log.DefaultLogger.FromContext(ctx)
	started := time.Now()

	// Retention only informs the choice and its notices. If it can't be read,
	// explicit grains run unchanged and Auto uses its documented fallback.
	retention, retentionErr := db.perfRetention(ctx)
	if retentionErr != nil && ctx.Err() != nil {
		return nil, retentionErr
	}
	retentionDuration := time.Since(started)
	choice := choosePerfGrain(q.Aggregation, retention, retentionErr, q.From, q.To, time.Now().UTC(), q.Budget)

	query, args, err := buildPerformanceQuery(q.CounterIDs, q.Object, q.CounterName, q.EntityIDs, choice.Aggregation, q.Statistic, q.From, q.To)
	if err != nil {
		return nil, err
	}
	view, _, _ := aggregationTable(choice.Aggregation)

	queryStarted := time.Now()
	series, rowCount, err := collectPerfSeries(ctx, db.Warehouse, query, args)
	if err != nil {
		return nil, err
	}
	queryDuration := time.Since(queryStarted)

	metaStarted := time.Now()
	var counters map[int64]perfCounterMeta
	var entities map[int64]perfEntityMeta
	if len(series) > 0 {
		counters, entities, err = loadPerfSeriesMetadata(ctx, db.Warehouse, series)
		if err != nil {
			return nil, fmt.Errorf("look up series names: %w", err)
		}
	}
	metaDuration := time.Since(metaStarted)

	frames, firstTS, lastTS := perfFrames(series, counters, entities, q.LegendFormat, q.FilterableValue)

	// Routine queries log at debug level. A fallback or a range that predates
	// retention logs at info, since that's what someone investigating missing
	// data needs to see. Ids, credentials and connection details are
	// deliberately left out.
	logFn := logger.Debug
	if retentionErr != nil || hasWarningNotice(choice.Notices) {
		logFn = logger.Info
	}
	r := retention[choice.Aggregation]
	logFn("SCOM performance query",
		"from", q.From.UTC().Format(time.RFC3339), "to", q.To.UTC().Format(time.RFC3339),
		"savedAggregation", string(q.Aggregation), "chosen", string(choice.Aggregation), "view", view, "reason", choice.Reason,
		"statistic", string(q.Statistic),
		"retentionAvailable", retentionErr == nil && len(retention) > 0,
		"maxDataAgeDays", r.MaxDataAgeDays, "intervalMinutes", r.IntervalMinutes, "lastGrooming", r.LastGrooming.Time.UTC().Format(time.RFC3339),
		"maxDataPoints", q.Budget.MaxDataPoints, "interval", q.Budget.Interval.String(), "targetBucket", choice.Target.String(),
		"firstSample", firstTS.UTC().Format(time.RFC3339), "lastSample", lastTS.UTC().Format(time.RFC3339),
		"rows", rowCount, "series", len(frames), "counterIds", len(q.CounterIDs), "entityIds", len(q.EntityIDs),
		"retentionDuration", retentionDuration.String(), "queryDuration", queryDuration.String(),
		"metadataDuration", metaDuration.String(), "totalDuration", time.Since(started).String(),
	)

	// Notices go on the first frame. With no data, an empty frame carries
	// them so the panel still explains why (e.g. range predates retention).
	if len(choice.Notices) > 0 {
		if len(frames) == 0 {
			frames = append(frames, data.NewFrame("").SetMeta(&data.FrameMeta{}))
		}
		frames[0].Meta.Notices = append(frames[0].Meta.Notices, choice.Notices...)
	}
	return frames, nil
}

// collectPerfSeries runs a buildPerformanceQuery query and groups its rows by
// series. NULL samples (raw SampleValue is nullable) carry nothing to plot
// and are skipped. rowCount counts every row read.
func collectPerfSeries(ctx context.Context, db *sql.DB, query string, args []any) (map[perfSeriesKey][]perfPoint, int, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	series := map[perfSeriesKey][]perfPoint{}
	rowCount := 0
	for rows.Next() {
		var key perfSeriesKey
		var ts time.Time
		var value sql.NullFloat64
		if err := rows.Scan(&key.counterRowID, &key.entityRowID, &ts, &value); err != nil {
			return nil, 0, err
		}
		rowCount++
		if !value.Valid {
			continue
		}
		series[key] = append(series[key], perfPoint{t: ts, v: value.Float64})
	}
	return series, rowCount, rows.Err()
}

// loadPerfSeriesMetadata runs buildPerformanceSeriesQuery for the series
// collectPerfSeries found.
func loadPerfSeriesMetadata(ctx context.Context, db *sql.DB, series map[perfSeriesKey][]perfPoint) (map[int64]perfCounterMeta, map[int64]perfEntityMeta, error) {
	counterSet := map[int64]bool{}
	entitySet := map[int64]bool{}
	for key := range series {
		counterSet[key.counterRowID] = true
		entitySet[key.entityRowID] = true
	}
	query, args := buildPerformanceSeriesQuery(sortedKeys(counterSet), sortedKeys(entitySet))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	counters := map[int64]perfCounterMeta{}
	for rows.Next() {
		var id int64
		var object, counter string
		var instance sql.NullString
		if err := rows.Scan(&id, &object, &counter, &instance); err != nil {
			return nil, nil, err
		}
		counters[id] = perfCounterMeta{object: object, counter: counter, instance: instance.String}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if !rows.NextResultSet() {
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("the series name lookup returned one result set, want two")
	}

	entities := map[int64]perfEntityMeta{}
	for rows.Next() {
		var id int64
		var guid, name string
		var host sql.NullString
		if err := rows.Scan(&id, &guid, &name, &host); err != nil {
			return nil, nil, err
		}
		entities[id] = perfEntityMeta{guid: guid, name: name, host: host.String}
	}
	return counters, entities, rows.Err()
}

func sortedKeys(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// perfFrames turns collected series into frames, sorted by legend (then by
// the series ids, so two series sharing a legend still keep their order),
// with each series' points in time order as the timeseries-multi contract
// requires. It also returns the first and last sample time, for logging.
func perfFrames(series map[perfSeriesKey][]perfPoint, counters map[int64]perfCounterMeta, entities map[int64]perfEntityMeta, legendFormat string, filterableValue bool) ([]*data.Frame, time.Time, time.Time) {
	type built struct {
		key    perfSeriesKey
		label  string
		labels data.Labels
		points []perfPoint
	}
	list := make([]built, 0, len(series))
	for key, points := range series {
		counterID := strconv.FormatInt(key.counterRowID, 10)
		c, ok := counters[key.counterRowID]
		if !ok {
			c = perfCounterMeta{object: "counter " + counterID}
		}
		e, ok := entities[key.entityRowID]
		if !ok {
			e = perfEntityMeta{name: fmt.Sprintf("entity %d", key.entityRowID)}
		}
		list = append(list, built{
			key:    key,
			label:  seriesLabel(legendFormat, c.object, c.counter, c.instance, e.name, e.host),
			labels: seriesLabels(counterID, e.guid, c.object, c.counter, c.instance, e.name, e.host),
			points: points,
		})
	}
	slices.SortFunc(list, func(a, b built) int {
		return cmp.Or(
			strings.Compare(strings.ToLower(a.label), strings.ToLower(b.label)),
			strings.Compare(a.label, b.label),
			cmp.Compare(a.key.counterRowID, b.key.counterRowID),
			cmp.Compare(a.key.entityRowID, b.key.entityRowID),
		)
	})

	var first, last time.Time
	frames := make([]*data.Frame, 0, len(list))
	for _, s := range list {
		slices.SortStableFunc(s.points, func(a, b perfPoint) int { return a.t.Compare(b.t) })
		times := make([]time.Time, len(s.points))
		values := make([]float64, len(s.points))
		for i, p := range s.points {
			times[i], values[i] = p.t, p.v
		}
		if len(times) > 0 {
			if first.IsZero() || times[0].Before(first) {
				first = times[0]
			}
			if times[len(times)-1].After(last) {
				last = times[len(times)-1]
			}
		}

		// Labels carry the series' dimensions in a form transformations can
		// still take apart (see seriesLabels); DisplayNameFromDS carries the
		// rendered legend. Both are needed: the field name alone ("value") is
		// what Grafana's time series panel uses for the legend, not the frame
		// name — without DisplayNameFromDS, every series in a multi-instance
		// query (e.g. Process\Working Set across every process) shows up as
		// the same indistinguishable "value", and with labels but no display
		// name it degrades instead to an auto-generated
		// value{object="...", counter="...", ...} dump.
		valueField := data.NewField("value", s.labels, values)
		valueField.Config = &data.FieldConfig{DisplayNameFromDS: s.label}
		frame := data.NewFrame(s.label,
			data.NewField("time", nil, times),
			valueField,
		)
		// Off by default: it duplicates every sample, and a frame carrying a
		// second numeric field is a shape some panels may read differently.
		if filterableValue {
			frame.Fields = append(frame.Fields, filterableValueField(values))
		}
		// Declaring the dataplane contract rather than leaving Grafana to
		// sniff the shape: one time field and one numeric field each, times
		// ascending per series, and label sets made unique by including the
		// series key — see seriesLabels.
		frame.Meta = &data.FrameMeta{
			Type:        data.FrameTypeTimeSeriesMulti,
			TypeVersion: data.FrameTypeVersion{0, 1},
		}
		frames = append(frames, frame)
	}
	return frames, first, last
}

func hasWarningNotice(notices []data.Notice) bool {
	for _, n := range notices {
		if n.Severity == data.NoticeSeverityWarning {
			return true
		}
	}
	return false
}
