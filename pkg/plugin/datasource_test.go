package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/a-logan/scom-datasource/pkg/scom"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestQueryData(t *testing.T) {
	ds := Datasource{}

	resp, err := ds.QueryData(
		context.Background(),
		&backend.QueryDataRequest{
			Queries: []backend.DataQuery{
				{RefID: "A"},
				{RefID: "B"},
			},
		},
	)
	if err != nil {
		t.Error(err)
	}

	if len(resp.Responses) != 2 {
		t.Fatal("QueryData must return a response for every query")
	}
	// An unparseable query is the request's fault, not the plugin's.
	if r := resp.Responses["A"]; r.Status != backend.StatusBadRequest || r.ErrorSource != backend.ErrorSourceDownstream {
		t.Errorf("got status %v source %q, want 400 downstream", r.Status, r.ErrorSource)
	}
}

func TestErrorResponse(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus backend.Status
		wantSource backend.ErrorSource
	}{
		{"invalid query", scom.BadRequestf("instance %q isn't a SCOM id", "$x"), backend.StatusBadRequest, backend.ErrorSourceDownstream},
		{"timeout", fmt.Errorf("resolve scope: %w", context.DeadlineExceeded), backend.StatusTimeout, backend.ErrorSourceDownstream},
		{"sql error", mssql.Error{Number: 208, Message: "Invalid object name 'x'."}, backend.StatusInternal, backend.ErrorSourceDownstream},
		{"plugin bug", errors.New("sql: Scan error on column index 3"), backend.StatusInternal, backend.ErrorSourcePlugin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := errorResponse(c.err)
			if r.Status != c.wantStatus || r.ErrorSource != c.wantSource {
				t.Errorf("got status %v source %q, want %v %q", r.Status, r.ErrorSource, c.wantStatus, c.wantSource)
			}
		})
	}
}

func TestHealthResult(t *testing.T) {
	details := func(r *backend.CheckHealthResult) string {
		t.Helper()
		var d map[string]string
		if err := json.Unmarshal(r.JSONDetails, &d); err != nil {
			t.Fatalf("details aren't JSON: %v", err)
		}
		return d["message"]
	}

	t.Run("both fine", func(t *testing.T) {
		r := healthResult(scom.HealthReport{})
		if r.Status != backend.HealthStatusOk || r.JSONDetails != nil {
			t.Errorf("got %+v, want a plain OK", r)
		}
	})

	// Both databases are reported, not just the first failure.
	t.Run("one failure reports both databases, with a hint", func(t *testing.T) {
		r := healthResult(scom.HealthReport{Warehouse: errors.New("can't read Perf.vPerfHourly: mssql: Invalid object name 'Perf.vPerfHourly'.")})
		if r.Status != backend.HealthStatusError || !strings.Contains(r.Message, "Data Warehouse") {
			t.Errorf("got %+v, want an error naming the Data Warehouse", r)
		}
		msg := details(r)
		for _, want := range []string{"Operational database: OK.", "Data Warehouse: can't read Perf.vPerfHourly", "OperationsManagerDW"} {
			if !strings.Contains(msg, want) {
				t.Errorf("details %q missing %q", msg, want)
			}
		}
	})

	t.Run("unreadable retention is OK with a warning", func(t *testing.T) {
		r := healthResult(scom.HealthReport{RetentionErr: errors.New("permission denied")})
		if r.Status != backend.HealthStatusOk {
			t.Errorf("got status %v, want OK: both databases work", r.Status)
		}
		if !strings.Contains(details(r), "dbo.StandardDatasetAggregation") {
			t.Errorf("details %q should say which tables need permission", details(r))
		}
	})
}

// The resource endpoints check ids before touching the database, so an
// unresolved dashboard variable comes back as a 400 with a clear message.
func TestResourceValidation(t *testing.T) {
	ds := Datasource{}
	for _, c := range []struct {
		path   string
		query  url.Values
		status int
	}{
		{"instances", url.Values{"classId": {"$class"}}, http.StatusBadRequest},
		{"counter-objects", url.Values{"instanceId": {"not-a-guid"}}, http.StatusBadRequest},
		{"nope", url.Values{}, http.StatusNotFound},
	} {
		_, err := ds.resource(context.Background(), c.path, c.query)
		if err == nil {
			t.Errorf("%s: got no error, want %d", c.path, c.status)
			continue
		}
		var notFound *unknownResourceError
		got := http.StatusInternalServerError
		switch {
		case errors.As(err, &notFound):
			got = http.StatusNotFound
		case scom.IsBadRequest(err):
			got = http.StatusBadRequest
		}
		if got != c.status {
			t.Errorf("%s: got status %d (%v), want %d", c.path, got, err, c.status)
		}
	}
}
