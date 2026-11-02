package scom

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestValidateIDs(t *testing.T) {
	if err := ValidateIDs("instance", "", "6F3D5B8A-1C2E-4A7B-9D10-AB12CD34EF56", "6f3d5b8a-1c2e-4a7b-9d10-ab12cd34ef56"); err != nil {
		t.Errorf("valid ids rejected: %v", err)
	}

	err := ValidateIDs("instance", "$server")
	if !IsBadRequest(err) {
		t.Fatalf("got %v, want a bad request error", err)
	}
	if !strings.Contains(err.Error(), "dashboard variable") {
		t.Errorf("message %q should point at the unresolved variable", err)
	}

	if err := ValidateIDs("class", "1; DROP TABLE x"); !IsBadRequest(err) {
		t.Errorf("got %v, want a bad request error", err)
	}
}

func TestParseCounterIDs(t *testing.T) {
	got, err := parseCounterIDs([]string{"12", " 7 "})
	if err != nil || len(got) != 2 || got[0] != 12 || got[1] != 7 {
		t.Fatalf("got (%v, %v), want ([12 7], nil)", got, err)
	}
	if _, err := parseCounterIDs([]string{"$counter"}); !IsBadRequest(err) {
		t.Errorf("got %v, want a bad request error", err)
	}
}

func TestIsDownstreamError(t *testing.T) {
	downstream := []error{
		mssql.Error{Number: 208, Message: "Invalid object name 'Perf.vPerfHourly'."},
		fmt.Errorf("resolve scope: %w", mssql.Error{Number: 18456, Message: "Login failed for user 'x'."}),
		context.DeadlineExceeded,
		errors.New("login error: mssql: Login failed for user 'grafana'"),
		errors.New("unable to open tcp connection with host 'sql:1433': dial tcp: connection refused"),
	}
	for _, err := range downstream {
		if !IsDownstreamError(err) {
			t.Errorf("IsDownstreamError(%v) = false, want true", err)
		}
	}
	for _, err := range []error{nil, errors.New("sql: Scan error on column index 3")} {
		if IsDownstreamError(err) {
			t.Errorf("IsDownstreamError(%v) = true, want false", err)
		}
	}
}

func TestConnectionHint(t *testing.T) {
	cases := []struct {
		err       string
		warehouse bool
		want      string
	}{
		{"can't read Perf.vPerfHourly: mssql: Invalid object name 'Perf.vPerfHourly'.", true, "OperationsManagerDW"},
		{"can't read dbo.BaseManagedEntity: mssql: Invalid object name 'dbo.BaseManagedEntity'.", false, "OperationsManager)"},
		// "Cannot open database" messages also say "The login failed": the
		// database hint must win.
		{`login error: mssql: Cannot open database "OpsMgr" requested by the login. The login failed.`, false, "can't open that database"},
		{"login error: mssql: Login failed for user 'grafana'.", false, "username, password"},
		{"TLS Handshake failed: x509: certificate signed by unknown authority", false, "Trust server certificate"},
		{"unable to open tcp connection with host 'sql:1433': dial tcp: lookup sql: no such host", false, "Host field"},
		{"unable to open tcp connection with host 'sql:1433': dial tcp 10.0.0.1:1433: i/o timeout", false, "firewalls"},
		{"mssql: The SELECT permission was denied on the object 'State'", false, "db_datareader"},
	}
	for _, c := range cases {
		got := ConnectionHint(errors.New(c.err), c.warehouse)
		if !strings.Contains(got, c.want) {
			t.Errorf("ConnectionHint(%q) = %q, want it to mention %q", c.err, got, c.want)
		}
	}
	if got := ConnectionHint(errors.New("something else"), false); got != "" {
		t.Errorf("got hint %q for an unknown error, want none", got)
	}
}
