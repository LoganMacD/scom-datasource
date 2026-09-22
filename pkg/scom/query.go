package scom

import (
	"context"
	"fmt"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
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

// QueryModel mirrors src/types.ts's MyQuery. It's what QueryData unmarshals
// each backend.DataQuery.JSON into.
type QueryModel struct {
	QueryType   QueryType     `json:"queryType"`
	Class       *ResourceRef  `json:"class"`
	Group       *ResourceRef  `json:"group"`
	Instances   []ResourceRef `json:"instances"`
	Object      *ResourceRef  `json:"object"`
	CounterName *ResourceRef  `json:"counterName"`
	Counters    []ResourceRef `json:"counters"`
	Aggregation Aggregation   `json:"aggregation"`
	// LegendFormat overrides the default performance series label — see
	// seriesLabel in performance.go. Empty (including on a query saved before
	// this field existed) keeps the built-in default.
	LegendFormat string `json:"legendFormat"`
	// PerformanceFormat picks a performance query's result shape — see
	// PerformanceFormat in performance.go. Empty (a query saved before this
	// field existed) is the time series shape it always returned.
	PerformanceFormat PerformanceFormat `json:"performanceFormat"`
	AllAlerts         bool              `json:"allAlerts"`
	Severities      []int64     `json:"severityFilter"`
	ResolutionState []int64     `json:"resolutionStateFilter"`
	AlertSource     AlertSource `json:"alertSource"`
	PropertyNames   []string    `json:"propertyNames"`
	HealthMode      HealthMode  `json:"healthMode"`
	// UnhealthyOnly prunes a health tree query's result down to monitors that
	// are themselves Warning/Critical, plus their ancestor chain up to the
	// root — see filterUnhealthyBranches in healthtree.go. Ignored by every
	// other query type.
	UnhealthyOnly bool `json:"unhealthyOnly"`
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
	// approximated LastModified).
	AlertSourceWarehouse AlertSource = "warehouse"
)

// HealthMode selects which of QueryHealthCurrent/QueryHealthHistory a health
// query runs. The zero value (unset, e.g. on a query saved before this field
// existed) behaves like HealthModeBoth, so old dashboards keep working
// unchanged.
type HealthMode string

const (
	HealthModeCurrent HealthMode = "current"
	HealthModeHistory HealthMode = "history"
	HealthModeBoth    HealthMode = "both"
)

// includesCurrent/includesHistory decide which of QueryHealthCurrent/
// QueryHealthHistory a HealthMode runs. Split out from the QueryTypeHealth
// case below so this on/off logic is directly testable — see query_test.go.
func (m HealthMode) includesCurrent() bool { return m != HealthModeHistory }
func (m HealthMode) includesHistory() bool { return m != HealthModeCurrent }

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
		return "", fmt.Errorf("health tree requires exactly one instance, got %d", len(instanceIDs))
	}
	return instanceIDs[0], nil
}

// Run dispatches a query model to the right domain query against the
// appropriate connection (Operational DB for alerts/health/properties, the
// Data Warehouse for performance). datasourceUID is this data source
// instance's own UID (from the request's PluginContext, threaded down from
// QueryData) — QueryHealthCurrent needs it to build the health tree
// drilldown link, which points back at this same data source.
func Run(ctx context.Context, db *DB, qm QueryModel, from, to time.Time, datasourceUID string) ([]*data.Frame, error) {
	instanceIDs := refValues(qm.Instances)
	counterIDs := refValues(qm.Counters)

	// A health tree query scoped to a group with no instances narrowed down
	// is rooted at the group itself — a SCOM group is itself a managed
	// entity with its own health rollup — with every member's monitor tree
	// hanging underneath, rather than requiring the user to pick one
	// specific member out of the group. The members have to be resolved and
	// passed down explicitly: a group entity's own dbo.State rows are just
	// the five standard System.Health rollups, so a tree built from the
	// group alone comes back the same three nodes regardless of what the
	// group contains. This must be checked before the general instance-scope
	// resolution below, which would flatten the group into its members and
	// lose the group as the root, tripping healthTreeInstanceID's "exactly
	// one instance" check on the way.
	if qm.QueryType == QueryTypeHealthTree && len(instanceIDs) == 0 && qm.Group != nil {
		memberIDs, err := ResolveInstanceIDs(ctx, db.Operational, "", qm.Group.Value)
		if err != nil {
			return nil, fmt.Errorf("resolve group members: %w", err)
		}
		return QueryHealthTree(ctx, db.Operational, qm.Group.Value, memberIDs, qm.UnhealthyOnly)
	}

	// No instances explicitly picked: fall back to every instance in the
	// chosen class/group scope, rather than treating it as "no data." This
	// is also needed for a performance query with an object/counter chosen
	// but no counter instances narrowed down (see below), so it only skips
	// the resolution when it's clearly not going to be used. An alerts query
	// with "all alerts" checked — or with no class/group/instance chosen at
	// all — is exempt too: it deliberately ignores scope entirely rather
	// than running against nothing.
	noScopeChosen := len(instanceIDs) == 0 && qm.Class == nil && qm.Group == nil
	allAlerts := qm.QueryType == QueryTypeAlerts && (qm.AllAlerts || noScopeChosen)
	needsInstanceScope := !allAlerts && (qm.QueryType != QueryTypePerformance ||
		(len(counterIDs) == 0 && qm.Object != nil && qm.CounterName != nil))
	if len(instanceIDs) == 0 && needsInstanceScope {
		var classID, groupID string
		if qm.Class != nil {
			classID = qm.Class.Value
		}
		if qm.Group != nil {
			groupID = qm.Group.Value
		}
		resolved, err := ResolveInstanceIDs(ctx, db.Operational, classID, groupID)
		if err != nil {
			return nil, fmt.Errorf("resolve instances: %w", err)
		}
		instanceIDs = resolved
	}

	switch qm.QueryType {
	case QueryTypePerformance:
		// A PerformanceRuleInstanceRowId (what counterIDs holds) is keyed on
		// (RuleRowId, InstanceName) alone — it's shared by every managed
		// entity that reports that rule with that instance name, not just
		// the ones the user selected. So the chosen instance/class/group
		// scope, widened to whatever each hosts/contains (performance
		// counters are almost never collected on the top-level instance
		// itself), must be passed down to QueryPerformance regardless of
		// whether counters were narrowed explicitly in the picker or left to
		// resolve automatically below — otherwise results include samples
		// from entities outside the user's selection.
		scopedIDs := instanceIDs
		if len(scopedIDs) > 0 {
			expanded, err := ExpandHostedEntityIDs(ctx, db.Operational, scopedIDs)
			if err != nil {
				return nil, fmt.Errorf("expand hosted entities: %w", err)
			}
			scopedIDs = expanded
		}
		// No specific counter instances narrowed down in the picker: an
		// object/counter selection alone means "every instance reporting
		// this counter" within that scope. QueryPerformance matches it by
		// name directly instead of resolving it to a (potentially huge) list
		// of rule instance ids first.
		var object, counterName string
		if len(counterIDs) == 0 && qm.Object != nil && qm.CounterName != nil {
			object, counterName = qm.Object.Value, qm.CounterName.Value
		}
		return QueryPerformance(ctx, db.Warehouse, counterIDs, object, counterName, scopedIDs, qm.Aggregation, qm.LegendFormat, qm.PerformanceFormat, from, to)

	case QueryTypeAlerts:
		// Alerts are almost always raised against the object that actually
		// misbehaved (a disk, a service, a SQL DB) rather than the top-level
		// instance/group member itself, so scope to whatever each selected
		// instance hosts/contains too — otherwise a group of Computers turns
		// up no alerts at all, since alerts live on their hosted children.
		scopedIDs := instanceIDs
		if !allAlerts && len(scopedIDs) > 0 {
			expanded, err := ExpandHostedEntityIDs(ctx, db.Operational, scopedIDs)
			if err != nil {
				return nil, fmt.Errorf("expand hosted entities: %w", err)
			}
			scopedIDs = expanded
		}
		filter := AlertFilter{
			InstanceIDs:     scopedIDs,
			Severities:      qm.Severities,
			ResolutionState: qm.ResolutionState,
			From:            from,
			To:              to,
		}
		var frame *data.Frame
		var err error
		if qm.AlertSource == AlertSourceWarehouse {
			frame, err = QueryAlertsWarehouse(ctx, db.Warehouse, db.Operational, filter)
		} else {
			frame, err = QueryAlerts(ctx, db.Operational, filter)
		}
		if err != nil || frame == nil {
			return nil, err
		}
		return []*data.Frame{frame}, nil

	case QueryTypeHealth:
		var frames []*data.Frame
		if qm.HealthMode.includesCurrent() {
			current, err := QueryHealthCurrent(ctx, db.Operational, instanceIDs, datasourceUID)
			if err != nil {
				return nil, err
			}
			if current != nil {
				frames = append(frames, current)
			}
		}
		if qm.HealthMode.includesHistory() {
			history, err := QueryHealthHistory(ctx, db.Operational, instanceIDs, from, to)
			if err != nil {
				return nil, err
			}
			if history != nil {
				frames = append(frames, history)
			}
		}
		return frames, nil

	case QueryTypeHealthTree:
		entityID, err := healthTreeInstanceID(instanceIDs)
		if err != nil {
			return nil, err
		}
		return QueryHealthTree(ctx, db.Operational, entityID, nil, qm.UnhealthyOnly)

	case QueryTypeProperties:
		return QueryProperties(ctx, db.Operational, instanceIDs, qm.PropertyNames)

	default:
		return nil, fmt.Errorf("unsupported query type: %q", qm.QueryType)
	}
}
