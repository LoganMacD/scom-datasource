package scom

import (
	"slices"
	"strconv"

	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// Colors for SCOM health states and alert severities. They're used by the
// opt-in stateColors query option (value mappings, so tables, stat panels
// and state timelines color themselves) and always by the health timeline.
const (
	colorHealthy      = "green"
	colorWarning      = "orange"
	colorCritical     = "red"
	colorNotMonitored = "gray"
	colorInformation  = "blue"
	colorMaintenance  = "blue"
)

// healthStateColor is the color of a numeric SCOM HealthState (0 Not
// Monitored, 1 Healthy, 2 Warning, 3 Critical).
func healthStateColor(code int64) string {
	switch code {
	case 1:
		return colorHealthy
	case 2:
		return colorWarning
	case 3:
		return colorCritical
	default:
		return colorNotMonitored
	}
}

// healthStateName is the standard name of a numeric HealthState, as
// healthStateCase falls back to when a monitor has no operational state name.
func healthStateName(code int64) string {
	switch code {
	case 0:
		return "Not Monitored"
	case 1:
		return "Healthy"
	case 2:
		return "Warning"
	case 3:
		return "Critical"
	default:
		return "Unknown"
	}
}

// valueColors is a value → color mapping for one field.
type valueColors map[string]string

// mappings turns valueColors into a Grafana value mapping. Values keep their
// own text; only the color is set. Index orders the entries the same way on
// every refresh.
func (vc valueColors) mappings() data.ValueMappings {
	if len(vc) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vc))
	for k := range vc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	mapper := data.ValueMapper{}
	for i, k := range keys {
		mapper[k] = data.ValueMappingResult{Color: vc[k], Index: i}
	}
	return data.ValueMappings{mapper}
}

// colorStateField applies a value mapping to a state field, and makes table
// panels show the mapped colors (as colored text). Unmapped values keep the
// normal text color rather than the default threshold green.
func colorStateField(f *data.Field, mappings data.ValueMappings) {
	if f.Config == nil {
		f.Config = &data.FieldConfig{}
	}
	f.Config.Mappings = mappings
	f.Config.Color = map[string]any{"mode": "fixed", "fixedColor": "text"}
	if f.Config.Custom == nil {
		f.Config.Custom = map[string]any{}
	}
	f.Config.Custom["cellOptions"] = map[string]any{"type": "color-text"}
}

// severityColors, priorityColors: the alert frame's fixed string values (see
// the CASE expressions in alerts.go).
var (
	severityColors = valueColors{"Critical": colorCritical, "Warning": colorWarning, "Information": colorInformation}
	priorityColors = valueColors{"High": colorWarning, "Low": colorNotMonitored}
)

// maintenanceModeMappings shows the in-maintenance flag as Yes/No, with Yes
// highlighted.
func maintenanceModeMappings() data.ValueMappings {
	return data.ValueMappings{data.ValueMapper{
		"true":  {Text: "Yes", Color: colorMaintenance, Index: 0},
		"false": {Text: "No", Index: 1},
	}}
}

// healthCodeMappings maps the numeric health states the health timeline
// returns to their names and colors.
func healthCodeMappings() data.ValueMappings {
	mapper := data.ValueMapper{}
	for code := int64(0); code <= 3; code++ {
		mapper[strconv.FormatInt(code, 10)] = data.ValueMappingResult{Text: healthStateName(code), Color: healthStateColor(code), Index: int(code)}
	}
	return data.ValueMappings{mapper}
}

// healthNameColors collects name → color for health state names seen in a
// result, from each row's numeric state. Names come from
// dbo.MonitorOperationalState and differ between monitors ("Success",
// "Error", ...), so the mapping is built from the data rather than a fixed
// list. The first color seen for a name wins.
type healthNameColors valueColors

func (h healthNameColors) add(name string, code int64) {
	if _, ok := h[name]; !ok {
		h[name] = healthStateColor(code)
	}
}
