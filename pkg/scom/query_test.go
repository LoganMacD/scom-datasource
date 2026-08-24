package scom

import "testing"

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
