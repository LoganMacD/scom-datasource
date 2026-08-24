package scom

// Option is the shape @grafana/ui's AsyncSelect/MultiSelect expect back from
// a resource picker endpoint.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
