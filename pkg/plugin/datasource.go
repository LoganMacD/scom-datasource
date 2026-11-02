package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-logan/scom-datasource/pkg/models"
	"github.com/a-logan/scom-datasource/pkg/scom"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/experimental/concurrent"
)

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

// maxConcurrentQueries is how many of one request's queries (a panel's
// queries A, B, C...) run at the same time. Kept small: each one can hit both
// SCOM databases, and every panel on a dashboard sends its own request.
const maxConcurrentQueries = 4

// NewDatasource creates a new datasource instance: it opens (but does not
// yet connect) both SQL Server connections for this data source instance.
func NewDatasource(_ context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	pluginSettings, err := models.LoadPluginSettings(settings)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	db, err := scom.Open(pluginSettings)
	if err != nil {
		return nil, fmt.Errorf("open connections: %w", err)
	}

	return &Datasource{db: db}, nil
}

// Datasource queries the SCOM Operational database and Data Warehouse
// directly over SQL Server.
type Datasource struct {
	db *scom.DB
}

func (d *Datasource) Dispose() {
	if d.db != nil {
		d.db.Close()
	}
}

// QueryData runs a request's queries in parallel (up to
// maxConcurrentQueries), each with its own response.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	return concurrent.QueryData(ctx, req, func(ctx context.Context, q concurrent.Query) backend.DataResponse {
		return d.query(ctx, datasourceUID(q.PluginContext), q.DataQuery)
	}, maxConcurrentQueries)
}

// datasourceUID is this data source instance's own UID, which the health
// tree drilldown link (see scom.QueryHealthCurrent) needs to target itself
// when opening Explore.
func datasourceUID(pc backend.PluginContext) string {
	if pc.DataSourceInstanceSettings == nil {
		return ""
	}
	return pc.DataSourceInstanceSettings.UID
}

func (d *Datasource) query(ctx context.Context, datasourceUID string, query backend.DataQuery) backend.DataResponse {
	var qm scom.QueryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		return backend.ErrDataResponseWithSource(backend.StatusBadRequest, backend.ErrorSourceDownstream, fmt.Sprintf("json unmarshal: %v", err))
	}
	qm.Budget = scom.PointBudget{MaxDataPoints: query.MaxDataPoints, Interval: query.Interval}

	frames, err := scom.Run(ctx, d.db, qm, query.TimeRange.From, query.TimeRange.To, datasourceUID)
	if err != nil {
		return errorResponse(err)
	}
	return backend.DataResponse{Frames: frames}
}

// errorResponse reports a failed query with an HTTP status, and tells
// Grafana whether the plugin or what it talks to is at fault: an invalid
// query (400) or a SQL Server, network or timeout failure is "downstream",
// anything else is a plugin error.
func errorResponse(err error) backend.DataResponse {
	switch {
	case scom.IsBadRequest(err):
		return backend.ErrDataResponseWithSource(backend.StatusBadRequest, backend.ErrorSourceDownstream, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return backend.ErrDataResponseWithSource(backend.StatusTimeout, backend.ErrorSourceDownstream, err.Error())
	case scom.IsDownstreamError(err):
		return backend.ErrDataResponseWithSource(backend.StatusInternal, backend.ErrorSourceDownstream, err.Error())
	default:
		return backend.ErrDataResponseWithSource(backend.StatusInternal, backend.ErrorSourcePlugin, err.Error())
	}
}

// CheckHealth checks both databases (see scom.DB.CheckHealth) and reports
// each one, with a hint for common failures, so the config page's Save &
// test says what to fix rather than just that something failed.
func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	return healthResult(d.db.CheckHealth(ctx)), nil
}

// healthResult turns a health report into what Save & test shows: a short
// summary, with details (Grafana's "message" key) describing each database.
func healthResult(r scom.HealthReport) *backend.CheckHealthResult {
	result := &backend.CheckHealthResult{Status: backend.HealthStatusOk}
	details := databaseResult("Operational database", r.Operational, false) + " " +
		databaseResult("Data Warehouse", r.Warehouse, true)

	switch {
	case r.Operational != nil && r.Warehouse != nil:
		result.Status = backend.HealthStatusError
		result.Message = "Couldn't connect to the SCOM Operational database or the Data Warehouse"
	case r.Operational != nil:
		result.Status = backend.HealthStatusError
		result.Message = "Couldn't connect to the SCOM Operational database"
	case r.Warehouse != nil:
		result.Status = backend.HealthStatusError
		result.Message = "Couldn't connect to the SCOM Data Warehouse"
	case r.RetentionErr != nil:
		result.Message = "Connected to the SCOM Operational database and Data Warehouse, but Auto aggregation can't read the retention settings"
		details = fmt.Sprintf("The warehouse account can't read the Data Warehouse retention settings (%v), so Auto aggregation will fall back to hourly or daily data. "+
			"Give it SELECT on dbo.StandardDataset, dbo.StandardDatasetAggregation and dbo.StandardDatasetAggregationStorage to fix this.", r.RetentionErr)
	default:
		result.Message = "Connected to the SCOM Operational database and Data Warehouse"
		return result
	}

	if body, err := json.Marshal(map[string]string{"message": details}); err == nil {
		result.JSONDetails = body
	}
	return result
}

func databaseResult(name string, err error, warehouse bool) string {
	if err == nil {
		return name + ": OK."
	}
	line := fmt.Sprintf("%s: %s.", name, strings.TrimSuffix(err.Error(), "."))
	if hint := scom.ConnectionHint(err, warehouse); hint != "" {
		line += " " + hint
	}
	return line
}

// CallResource backs the query editor's searchable dropdowns (classes,
// groups, instances, performance counters, property names, resolution
// states) and the variable editor's value lists. Every endpoint returns a
// JSON array of {value, label[, description]}.
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	u, err := url.Parse(req.URL)
	if err != nil {
		return sendJSONError(sender, http.StatusBadRequest, err)
	}

	opts, err := d.resource(ctx, strings.TrimPrefix(req.Path, "/"), u.Query())
	if err != nil {
		status := http.StatusInternalServerError
		var notFound *unknownResourceError
		switch {
		case errors.As(err, &notFound):
			status = http.StatusNotFound
		case scom.IsBadRequest(err):
			status = http.StatusBadRequest
		}
		return sendJSONError(sender, status, err)
	}

	// json.Marshal(nil slice) produces "null", which breaks the frontend
	// pickers (they call .map on the parsed result) whenever a search
	// legitimately matches zero rows. Always send a JSON array.
	if opts == nil {
		opts = []scom.Option{}
	}

	body, err := json.Marshal(opts)
	if err != nil {
		return err
	}

	return sender.Send(&backend.CallResourceResponse{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
		},
		Body: body,
	})
}

type unknownResourceError struct{ path string }

func (e *unknownResourceError) Error() string { return "unknown resource path: " + e.path }

func (d *Datasource) resource(ctx context.Context, path string, q url.Values) ([]scom.Option, error) {
	search := q.Get("search")
	classID, groupID, instanceIDs := q.Get("classId"), q.Get("groupId"), q["instanceId"]
	if err := scom.ValidateIDs("class", classID); err != nil {
		return nil, err
	}
	if err := scom.ValidateIDs("group", groupID); err != nil {
		return nil, err
	}
	if err := scom.ValidateIDs("instance", instanceIDs...); err != nil {
		return nil, err
	}

	switch path {
	case "classes":
		return scom.SearchClasses(ctx, d.db, search)

	case "groups":
		return scom.SearchGroups(ctx, d.db, search)

	case "instances":
		return scom.SearchInstances(ctx, d.db.Operational, scom.InstanceFilter{
			ClassID: classID,
			GroupID: groupID,
			Search:  search,
		})

	case "counter-objects", "counter-names", "counters":
		// Performance counters are almost never collected on the top-level
		// instance itself, but on the objects it hosts (disks, SQL DB
		// engines, processes, ...), so the pickers search a hosting-expanded
		// sample of the selection — see scom.DB.CounterPickerScope.
		scope, err := d.db.CounterPickerScope(ctx, classID, groupID, instanceIDs)
		if err != nil {
			return nil, err
		}
		switch path {
		case "counter-objects":
			return scom.SearchCounterObjects(ctx, d.db, scope, search)
		case "counter-names":
			return scom.SearchCounterNames(ctx, d.db, scope, q.Get("object"), search)
		default:
			return scom.SearchCounterInstances(ctx, d.db, scope, q.Get("object"), q.Get("counterName"), search)
		}

	case "properties":
		if classID == "" {
			return []scom.Option{}, nil
		}
		return scom.ListPropertyNames(ctx, d.db, classID)

	case "resolution-states":
		return d.db.ResolutionStates(ctx)

	case "variable-values":
		// A dashboard variable's values: every class, every group, or the
		// instances of a class or group (text = name, value = id).
		switch q.Get("type") {
		case "classes":
			return d.db.ClassOptions(ctx)
		case "groups":
			return d.db.GroupOptions(ctx)
		case "instances":
			return d.db.InstanceOptions(ctx, classID, groupID)
		default:
			return nil, scom.BadRequestf("unknown variable type %q", q.Get("type"))
		}

	default:
		return nil, &unknownResourceError{path: path}
	}
}

func sendJSONError(sender backend.CallResourceResponseSender, status int, err error) error {
	body, _ := json.Marshal(map[string]string{"error": err.Error()})
	return sender.Send(&backend.CallResourceResponse{
		Status: status,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
		},
		Body: body,
	})
}
