package scom

import (
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

func TestHealthStateColor(t *testing.T) {
	cases := map[int64]string{0: colorNotMonitored, 1: colorHealthy, 2: colorWarning, 3: colorCritical, 9: colorNotMonitored}
	for code, want := range cases {
		if got := healthStateColor(code); got != want {
			t.Errorf("healthStateColor(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestValueColorsMappings(t *testing.T) {
	mappings := severityColors.mappings()
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings, want one value mapper", len(mappings))
	}
	mapper, ok := mappings[0].(data.ValueMapper)
	if !ok {
		t.Fatalf("got %T, want data.ValueMapper", mappings[0])
	}
	if mapper["Critical"].Color != colorCritical || mapper["Warning"].Color != colorWarning {
		t.Errorf("got %+v, want Critical red and Warning orange", mapper)
	}
	// Values keep their own text; only the color is mapped.
	if mapper["Critical"].Text != "" {
		t.Errorf("got text %q, want the value's own text kept", mapper["Critical"].Text)
	}
}

// Health state names come from each monitor's operational states ("Success",
// "Error", ...), so colors are keyed on the names actually returned.
func TestHealthNameColors(t *testing.T) {
	colors := healthNameColors{}
	colors.add("Success", 1)
	colors.add("Error", 3)
	colors.add("Success", 3) // first color wins
	if colors["Success"] != colorHealthy || colors["Error"] != colorCritical {
		t.Errorf("got %v", colors)
	}
}

func TestColorStateField(t *testing.T) {
	f := data.NewField("severity", nil, []string{"Critical"})
	colorStateField(f, severityColors.mappings())
	if len(f.Config.Mappings) == 0 {
		t.Fatal("no mappings set")
	}
	cell, ok := f.Config.Custom["cellOptions"].(map[string]any)
	if !ok || cell["type"] != "color-text" {
		t.Errorf("got custom config %v, want a color-text table cell", f.Config.Custom)
	}
}

func TestHealthCodeMappings(t *testing.T) {
	mapper := healthCodeMappings()[0].(data.ValueMapper)
	if r := mapper["3"]; r.Text != "Critical" || r.Color != colorCritical {
		t.Errorf("got %+v for state 3, want Critical in red", r)
	}
	if r := mapper["1"]; r.Text != "Healthy" || r.Color != colorHealthy {
		t.Errorf("got %+v for state 1, want Healthy in green", r)
	}
}
