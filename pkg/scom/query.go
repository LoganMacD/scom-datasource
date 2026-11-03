package scom

import (
	"context"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"golang.org/x/sync/errgroup"
)

type QueryType string

const (
	QueryTypePerformance QueryType = "performance"
	QueryTypeAlerts      QueryType = "alerts"
	QueryTypeHealth      QueryType = "health"
	QueryTypeProperties  QueryType = "properties"
	// QueryTypeHealthTree renders one managed entity's full SCOM monitor
	// hierarchy (like SCOM's own Health Explorer) via QueryHealthTree. Unlike
	// every other query type, it requires exactly one instance — see
	// healthTreeInstanceID.
	QueryTypeHealthTree QueryType = "health-tree"
)

// ResourceRef mirrors src/types.ts's ResourceRef: a {value,label} pair so
// the frontend can redisplay a saved selection without re-resolving its id.
// Only Value is used on the backend.
type ResourceRef struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (r *ResourceRef) value() string {
	if r == nil {
		return ""
	}
	return r.Value
}

// QueryModel mirrors src/types.ts's MyQuery. It's what QueryData unmarshals
// each backend.DataQuery.JSON into.
//
// New fields must keep the old behaviour when unset, because saved
// dashboards don't have them — see the field comments.
type QueryModel struct {
	QueryType   QueryType     `json:"queryType"`
	Class       *ResourceRef  `json:"class"`
	Group       *ResourceRef  `json:"group"`
	Instances   []ResourceRef `json:"instances"`
	Object      *ResourceRef  `json:"object"`
	CounterName *ResourceRef  `json:"counterName"`
	Counters    []ResourceRef `json:"counters"`
	Aggregation Aggregation   `json:"aggregation"`
	// Statistic picks the hourly/daily statistic to plot; "" (any query saved
	// before this field existed) is the average — see Statistic.
	Statistic Statistic `json:"statistic"`
	// LegendFormat overrides the default performance series label — see
	// seriesLabel in performance.go. Empty (including on a query saved before
	// this field existed) keeps the built-in default.
	LegendFormat string `json:"legendFormat"`
	// FilterableValue adds a hidden second copy of each performance series'
	// values, displayed as "value" in every frame so "Filter data by values"
	// can target it — see filterableValueField in performance.go. False (the
	// default, and any query saved before this field existed) omits it.
	FilterableValue bool        `json:"filterableValue"`
	AllAlerts       bool        `json:"allAlerts"`
	Severities      []int64     `json:"severityFilter"`
	ResolutionState []int64     `json:"resolutionStateFilter"`
	AlertSource     AlertSource `json:"alertSource"`
	// AlertTimeFilter picks how an alerts query uses the time range; ""
	// (any query saved before this field existed) is AlertTimeRaised.
	AlertTimeFilter AlertTimeFilter `json:"alertTimeFilter"`
	PropertyNames   []string        `json:"propertyNames"`
	HealthMode      HealthMode      `json:"healthMode"`
	// UnhealthyOnly prunes a health tree query's result down to monitors that
	// are themselves Warning/Critical, plus their ancestor chain up to the
	// root — see healthTree.keep in healthtree.go. Ignored by every other
	// query type.
	UnhealthyOnly bool `json:"unhealthyOnly"`
	// StateColors adds value mappings that color health states, alert
	// severities and priorities, and show maintenance mode as Yes/No — see
	// colors.go. New queries turn it on (DEFAULT_QUERY in src/types.ts);
	// false, including any query saved before it existed, leaves fields as
	// they were. It's opt-in because a data source's mappings take priority
	// over a panel's own default mappings.
	StateColors bool `json:"stateColors"`
	// Budget comes from backend.DataQuery's MaxDataPoints/Interval (set in
	// pkg/plugin/datasource.go), not from the query JSON. Auto aggregation
	// uses it to pick a grain — see choosePerfGrain.
	Budget PointBudget `json:"-"`
}

// AlertSource selects which database an alerts query reads from. The zero
// value (unset, e.g. a query saved before this field existed) behaves like
// AlertSourceOperational, keeping old dashboards pointed at the same data
// they always were.
type AlertSource string

const (
	AlertSourceOperational AlertSource = "operational"
	// AlertSourceWarehouse reads Alert.vAlert on the Data Warehouse instead,
	// which SCOM retains far longer than the Operational database's alert
	// history — see QueryAlertsWarehouse for the tradeoffs (no Owner field,
	// approximated LastModified, raised-in-range only).
	AlertSourceWarehouse AlertSource = "warehouse"
)

// HealthMode selects what a health query returns. The zero value (unset,
// e.g. on a query saved before this field existed) behaves like
// HealthModeBoth, so old dashboards keep working unchanged.
type HealthMode string

const (
	HealthModeCurrent HealthMode = "current"
	HealthModeHistory HealthMode = "history"
	HealthModeBoth    HealthMode = "both"
	// HealthModeTimeline returns each entity's state over the time range for
	// a State timeline panel — see QueryHealthTimeline. It returns neither
	// the current nor the history frame.
	HealthModeTimeline HealthMode = "timeline"
)

// includesCurrent/includesHistory decide which of QueryHealthCurrent/
// QueryHealthHistory a HealthMode runs. Split out from the QueryTypeHealth
// case below so this on/off logic is directly testable — see query_test.go.
func (m HealthMode) includesCurrent() bool { return m != HealthModeHistory && m != HealthModeTimeline }
func (m HealthMode) includesHistory() bool { return m != HealthModeCurrent && m != HealthModeTimeline }

func refValues(refs []ResourceRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Value
	}
	return out
}

// healthTreeInstanceID picks the single instance a health tree query is
// scoped to. Split out from the QueryTypeHealthTree case below so this
// validation is directly testable without a live DB — see query_test.go.
// Unlike every other query type (which treats "no instances narrowed"
// as "every instance in scope"), a health tree is inherently about one
// object, so both zero and more-than-one are rejected rather than silently
// picking a subset.
func healthTreeInstanceID(instanceIDs []string) (string, error) {
	if len(instanceIDs) != 1 {
		return "", badRequestf("health tree requires exactly one instance, got %d", len(instanceIDs))
	}
	return instanceIDs[0], nil
}

// validate checks a query's ids and options before anything runs, so a bad
// value (usually a dashboard variable that didn't resolve) fails with a clear
// message instead of a SQL conversion error. Drilldown links bypass the
// frontend's checks, so this has to happen here.
func (qm QueryModel) validate() error {
	if err := ValidateIDs("class", qm.Class.value()); err != nil {
		return err
	}
	if err := ValidateIDs("group", qm.Group.value()); err != nil {
		return err
	}
	if err := ValidateIDs("instance", refValues(qm.Instances)...); err != nil {
		return err
	}
	if !validStatistic(qm.Statistic) {
		return badRequestf("unsupported statistic: %q", qm.Statistic)
	}
	if !validAlertTimeFilter(qm.AlertTimeFilter) {
		return badRequestf("unsupported alert time filter: %q", qm.AlertTimeFilter)
	}
	return nil
}

// emptyScopeNotice explains an empty result when the chosen class, group or
// instances match nothing. Before this check, an empty scope applied no
// filter at all, so performance and alert queries returned every entity's
// data.
var emptyScopeNotice = data.Notice{
	Severity: data.NoticeSeverityInfo,
	Text:     "No SCOM objects match the selected class, group or instances, so there's nothing to show.",
}

func noticeFrame(notice data.Notice) *data.Frame {
	return data.NewFrame("").SetMeta(&data.FrameMeta{Notices: []data.Notice{notice}})
}

// Run dispatches a query model to the right domain query against the
// appropriate connection (Operational DB for alerts/health/properties, the
// Data Warehouse for performance). datasourceUID is this data source
// instance's own UID (from the request's PluginContext, threaded down from
// QueryData) — QueryHealthCurrent needs it to build the health tree
// drilldown link, which points back at this same data source.
//
// No instances picked means every instance in the chosen class/group scope,
// rather than "no data". A scope that matches nothing returns an explanatory
// notice instead of unfiltered results.
func Run(ctx context.Context, db *DB, qm QueryModel, from, to time.Time, datasourceUID string) ([]*data.Frame, error) {
	if err := qm.validate(); err != nil {
		return nil, err
	}
	instanceIDs := refValues(qm.Instances)
	classID, groupID := qm.Class.value(), qm.Group.value()
	scopeChosen := len(instanceIDs) > 0 || classID != "" || groupID != ""

	switch qm.QueryType {
	case QueryTypePerformance:
		return runPerformance(ctx, db, qm, classID, groupID, instanceIDs, scopeChosen, from, to)

	case QueryTypeAlerts:
		return runAlerts(ctx, db, qm, classID, groupID, instanceIDs, scopeChosen, from, to)

	case QueryTypeHealth:
		ids, err := db.InstanceScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve instances: %w", err)
		}
		if scopeChosen && len(ids) == 0 {
			return []*data.Frame{noticeFrame(emptyScopeNotice)}, nil
		}
		return runHealth(ctx, db, qm, ids, from, to, datasourceUID)

	case QueryTypeHealthTree:
		// A health tree query scoped to a group with no instances narrowed
		// down is rooted at the group itself — a SCOM group is itself a
		// managed entity — with its members underneath, rather than requiring
		// the user to pick one specific member out of the group. Members sit
		// under the group's rollup monitors where SCOM rolls their health up,
		// and directly under the group otherwise (see newHealthTree). They're
		// resolved and passed down explicitly because a group needn't have
		// any rollup monitors: its own health is then Not Monitored and says
		// nothing about what it contains. This must be checked before the
		// general instance-scope resolution below, which would flatten the
		// group into its members and lose the group as the root, tripping
		// healthTreeInstanceID's "exactly one instance" check on the way.
		if len(instanceIDs) == 0 && groupID != "" {
			members, err := db.InstanceScope(ctx, "", groupID, nil)
			if err != nil {
				return nil, fmt.Errorf("resolve group members: %w", err)
			}
			return QueryHealthTree(ctx, db.Operational, groupID, members, qm.UnhealthyOnly)
		}
		ids, err := db.InstanceScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve instances: %w", err)
		}
		entityID, err := healthTreeInstanceID(ids)
		if err != nil {
			return nil, err
		}
		return QueryHealthTree(ctx, db.Operational, entityID, nil, qm.UnhealthyOnly)

	case QueryTypeProperties:
		ids, err := db.InstanceScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve instances: %w", err)
		}
		if scopeChosen && len(ids) == 0 {
			return []*data.Frame{noticeFrame(emptyScopeNotice)}, nil
		}
		return QueryProperties(ctx, db, ids, qm.PropertyNames)

	default:
		return nil, badRequestf("unsupported query type: %q", qm.QueryType)
	}
}

func runPerformance(ctx context.Context, db *DB, qm QueryModel, classID, groupID string, instanceIDs []string, scopeChosen bool, from, to time.Time) ([]*data.Frame, error) {
	counterIDs, err := parseCounterIDs(refValues(qm.Counters))
	if err != nil {
		return nil, err
	}
	// No specific counter instances narrowed down in the picker: an
	// object/counter selection alone means "every instance reporting this
	// counter" within the scope, matched by name (see buildPerformanceQuery).
	var object, counterName string
	if len(counterIDs) == 0 && qm.Object != nil && qm.CounterName != nil {
		object, counterName = qm.Object.Value, qm.CounterName.Value
	}
	if len(counterIDs) == 0 && (object == "" || counterName == "") {
		return nil, nil
	}

	// A PerformanceRuleInstanceRowId (what counterIDs holds) is keyed on
	// (RuleRowId, InstanceName) alone — it's shared by every managed entity
	// that reports that rule with that instance name, not just the ones the
	// user selected. So the chosen scope, widened to whatever each instance
	// hosts or contains (performance counters are almost never collected on
	// the top-level instance itself), is applied whether counters were picked
	// explicitly or matched by name. Otherwise results include samples from
	// entities outside the selection.
	var entityIDs []string
	if scopeChosen {
		expanded, err := db.ExpandedScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve scope: %w", err)
		}
		if len(expanded) == 0 {
			return []*data.Frame{noticeFrame(emptyScopeNotice)}, nil
		}
		entityIDs = expanded
	}

	return QueryPerformance(ctx, db, PerformanceQuery{
		CounterIDs:      counterIDs,
		Object:          object,
		CounterName:     counterName,
		EntityIDs:       entityIDs,
		Aggregation:     qm.Aggregation,
		Statistic:       qm.Statistic,
		LegendFormat:    qm.LegendFormat,
		FilterableValue: qm.FilterableValue,
		From:            from,
		To:              to,
		Budget:          qm.Budget,
	})
}

func runAlerts(ctx context.Context, db *DB, qm QueryModel, classID, groupID string, instanceIDs []string, scopeChosen bool, from, to time.Time) ([]*data.Frame, error) {
	// "All alerts", or nothing scoped at all, deliberately ignores scope.
	// Otherwise alerts are scoped to whatever each selected instance hosts or
	// contains too: alerts are almost always raised against the object that
	// misbehaved (a disk, a service, a SQL DB) rather than the top-level
	// instance, so a group of computers would otherwise show no alerts.
	var scopedIDs []string
	if !qm.AllAlerts && scopeChosen {
		expanded, err := db.ExpandedScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, fmt.Errorf("resolve scope: %w", err)
		}
		if len(expanded) == 0 {
			return []*data.Frame{emptyAlertsFrame(qm.StateColors)}, nil
		}
		scopedIDs = expanded
	}

	filter := AlertFilter{
		InstanceIDs:     scopedIDs,
		Severities:      qm.Severities,
		ResolutionState: qm.ResolutionState,
		TimeFilter:      qm.AlertTimeFilter,
		From:            from,
		To:              to,
	}
	var frame *data.Frame
	var err error
	if qm.AlertSource == AlertSourceWarehouse {
		frame, err = QueryAlertsWarehouse(ctx, db, filter, qm.StateColors)
	} else {
		frame, err = QueryAlerts(ctx, db.Operational, filter, qm.StateColors)
	}
	if err != nil {
		return nil, err
	}
	return []*data.Frame{frame}, nil
}

// runHealth runs a health query's frames. Current state and history are
// independent queries, so with both they run at the same time.
func runHealth(ctx context.Context, db *DB, qm QueryModel, ids []string, from, to time.Time, datasourceUID string) ([]*data.Frame, error) {
	if qm.HealthMode == HealthModeTimeline {
		return QueryHealthTimeline(ctx, db.Operational, ids, from, to)
	}

	var current, history *data.Frame
	g, gctx := errgroup.WithContext(ctx)
	if qm.HealthMode.includesCurrent() {
		g.Go(func() error {
			var err error
			current, err = QueryHealthCurrent(gctx, db.Operational, ids, datasourceUID, qm.StateColors)
			return err
		})
	}
	if qm.HealthMode.includesHistory() {
		g.Go(func() error {
			var err error
			history, err = QueryHealthHistory(gctx, db.Operational, ids, from, to, qm.StateColors)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var frames []*data.Frame
	if current != nil {
		frames = append(frames, current)
	}
	if history != nil {
		frames = append(frames, history)
	}
	return frames, nil
}
