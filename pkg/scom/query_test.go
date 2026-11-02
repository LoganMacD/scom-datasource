package scom

import (
	"testing"
)

// TestHealthModeIncludes pins the on/off logic dispatching QueryHealthCurrent
// and QueryHealthHistory. Crucially, the zero value (a query saved before
// HealthMode existed, so its JSON has no healthMode field) must behave like
// "both" — old dashboards should keep returning exactly what they did before
// this option was added.
func TestHealthModeIncludes(t *testing.T) {
	cases := []struct {
		mode        HealthMode
		wantCurrent bool
		wantHistory bool
	}{
		{HealthMode(""), true, true}, // zero value / unset on old saved queries
		{HealthModeBoth, true, true},
		{HealthModeCurrent, true, false},
		{HealthModeHistory, false, true},
	}
	for _, c := range cases {
		if got := c.mode.includesCurrent(); got != c.wantCurrent {
			t.Errorf("HealthMode(%q).includesCurrent() = %v, want %v", c.mode, got, c.wantCurrent)
		}
		if got := c.mode.includesHistory(); got != c.wantHistory {
			t.Errorf("HealthMode(%q).includesHistory() = %v, want %v", c.mode, got, c.wantHistory)
		}
	}
}

// TestHealthTreeInstanceID pins the "exactly one instance" rule a health
// tree query enforces server-side — unlike every other query type, which
// treats zero instances as "everything in scope," a health tree is
// inherently about a single object. This backend check matters even though
// the frontend's filterQuery also guards it, since a drilldown link's Query
// payload (see healthTreeDrilldownLink in health.go) bypasses the frontend
// entirely.
func TestHealthTreeInstanceID(t *testing.T) {
	cases := []struct {
		name        string
		instanceIDs []string
		wantID      string
		wantErr     bool
	}{
		{"zero instances errors", nil, "", true},
		{"exactly one instance succeeds", []string{"inst-1"}, "inst-1", false},
		{"more than one instance errors", []string{"inst-1", "inst-2"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := healthTreeInstanceID(c.instanceIDs)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got no error for %d instances, want one", len(c.instanceIDs))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != c.wantID {
				t.Errorf("got id=%q, want %q", id, c.wantID)
			}
		})
	}
}

// The timeline mode returns only the timeline, never the current/history frames.
func TestHealthModeTimeline(t *testing.T) {
	if HealthModeTimeline.includesCurrent() || HealthModeTimeline.includesHistory() {
		t.Error("timeline mode also runs current/history, want neither")
	}
}

func TestQueryModelValidate(t *testing.T) {
	valid := "6f3d5b8a-1c2e-4a7b-9d10-ab12cd34ef56"
	cases := []struct {
		name    string
		qm      QueryModel
		wantErr bool
	}{
		{"empty query", QueryModel{}, false},
		{"valid ids", QueryModel{Class: &ResourceRef{Value: valid}, Instances: []ResourceRef{{Value: valid}}}, false},
		{"unresolved variable as instance", QueryModel{Instances: []ResourceRef{{Value: "$server"}}}, true},
		{"bad group id", QueryModel{Group: &ResourceRef{Value: "not-a-guid"}}, true},
		{"unknown statistic", QueryModel{Statistic: "median"}, true},
		{"known statistic", QueryModel{Statistic: StatisticMax}, false},
		{"unknown alert time filter", QueryModel{AlertTimeFilter: "sometimes"}, true},
		{"known alert time filter", QueryModel{AlertTimeFilter: AlertTimeActive}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.qm.validate()
			if c.wantErr != (err != nil) {
				t.Fatalf("validate() = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil && !IsBadRequest(err) {
				t.Errorf("got %T, want a bad request error", err)
			}
		})
	}
}

func TestHealthTreeInstanceIDIsBadRequest(t *testing.T) {
	if _, err := healthTreeInstanceID(nil); !IsBadRequest(err) {
		t.Errorf("got %v, want a bad request error", err)
	}
}
