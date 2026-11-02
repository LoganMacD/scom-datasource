package scom

// Option is one entry in a picker or variable list: what the query editor's
// Combobox options and Grafana's variable values are built from. Description
// is optional extra text shown under the label (a class's technical name,
// for example).
type Option struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
