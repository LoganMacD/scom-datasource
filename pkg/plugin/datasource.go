package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a-logan/scom-datasource/pkg/models"
	"github.com/a-logan/scom-datasource/pkg/scom"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
)

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

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
	d.db.Close()
}

func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()

	// The health tree drilldown link (see scom.QueryHealthCurrent) needs this
	// data source instance's own UID to target itself when opening Explore.
	var datasourceUID string
	if req.PluginContext.DataSourceInstanceSettings != nil {
		datasourceUID = req.PluginContext.DataSourceInstanceSettings.UID
	}

	for _, q := range req.Queries {
		response.Responses[q.RefID] = d.query(ctx, datasourceUID, q)
	}

	return response, nil
}

func (d *Datasource) query(ctx context.Context, datasourceUID string, query backend.DataQuery) backend.DataResponse {
	var qm scom.QueryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

	frames, err := scom.Run(ctx, d.db, qm, query.TimeRange.From, query.TimeRange.To, datasourceUID)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusInternal, err.Error())
	}

	var response backend.DataResponse
	response.Frames = frames
	return response
}

// CheckHealth pings both connections and probes a known table/view on each
// so schema or permission problems surface on the config page's Test button,
// not on the first real dashboard query.
func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if err := d.db.CheckHealth(ctx); err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: err.Error(),
		}, nil
	}

	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Connected to the SCOM Operational database and Data Warehouse",
	}, nil
}

// CallResource backs the query editor's searchable dropdowns: classes,
// groups, instances, performance counters, and property names.
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	u, err := url.Parse(req.URL)
	if err != nil {
		return sendJSONError(sender, http.StatusBadRequest, err)
	}
	q := u.Query()
	search := q.Get("search")

	var (
		opts []scom.Option
		errQ error
	)

	switch strings.TrimPrefix(req.Path, "/") {
	case "classes":
		opts, errQ = scom.SearchClasses(ctx, d.db.Operational, search, scom.SearchBy(q.Get("by")))

	case "groups":
		opts, errQ = scom.SearchGroups(ctx, d.db.Operational, search)

	case "instances":
		opts, errQ = scom.SearchInstances(ctx, d.db.Operational, scom.InstanceFilter{
			ClassID: q.Get("classId"),
			GroupID: q.Get("groupId"),
			Search:  search,
		})

	case "counter-objects":
		var instanceIDs []string
		instanceIDs, errQ = d.resolveCounterScopeIDs(ctx, q)
		if errQ == nil {
			opts, errQ = scom.SearchCounterObjects(ctx, d.db.Warehouse, instanceIDs, search)
		}

	case "counter-names":
		var instanceIDs []string
		instanceIDs, errQ = d.resolveCounterScopeIDs(ctx, q)
		if errQ == nil {
			opts, errQ = scom.SearchCounterNames(ctx, d.db.Warehouse, instanceIDs, q.Get("object"), search)
		}

	case "counters":
		var instanceIDs []string
		instanceIDs, errQ = d.resolveCounterScopeIDs(ctx, q)
		if errQ == nil {
			opts, errQ = scom.SearchCounterInstances(ctx, d.db.Warehouse, instanceIDs, q.Get("object"), q.Get("counterName"), search)
		}

	case "properties":
		opts, errQ = scom.ListPropertyNames(ctx, d.db.Operational, q.Get("classId"))

	case "resolution-states":
		opts, errQ = scom.ListResolutionStates(ctx, d.db.Operational)

	default:
		return sendJSONError(sender, http.StatusNotFound, fmt.Errorf("unknown resource path: %s", req.Path))
	}

	if errQ != nil {
		return sendJSONError(sender, http.StatusInternalServerError, errQ)
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

// resolveCounterScopeIDs figures out which top-level instances the counter
// pickers (objects/names/instances) should be scoped to: explicit instanceId
// params if given, else every instance in the chosen class/group scope
// (scom.ResolveInstanceIDs), then widened to whatever each of those
// hosts/contains (scom.ExpandHostedEntityIDs) — performance counters are
// almost never collected on the top-level instance itself, but on the
// objects it hosts (disks, SQL DB engines, processes, ...).
func (d *Datasource) resolveCounterScopeIDs(ctx context.Context, q url.Values) ([]string, error) {
	instanceIDs := q["instanceId"]
	if len(instanceIDs) == 0 {
		resolved, err := scom.ResolveInstanceIDs(ctx, d.db.Operational, q.Get("classId"), q.Get("groupId"))
		if err != nil {
			return nil, err
		}
		instanceIDs = resolved
	}
	if len(instanceIDs) == 0 {
		return nil, nil
	}
	// A whole class/group, or a large explicit selection, is compared via its
	// first few instances only — see scom.CounterPickerSampleSize.
	instanceIDs = scom.SampleCounterScopeIDs(instanceIDs)
	return scom.ExpandHostedEntityIDs(ctx, d.db.Operational, instanceIDs)
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
