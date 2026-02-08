package v1alpha1

// StructuredQuery defines a label-agnostic metric query builder.
// This is designed to work across organizations and metric conventions:
// - metric names are arbitrary Prometheus metric names (service metrics, Istio, Envoy, etc.)
// - labels are dynamic key/value selectors, with optional regex matching
// - only counter, histogram, and summary metric types are supported
// - query methods are limited to rate, count, and quantile
// - optional groupBy dimensions allow per-entity evaluation (e.g., per-pod)
//
// The controller MUST translate this structured definition into valid PromQL
// during rule evaluation.
//
// Admission policy:
// - Metric.name must be non-empty.
// - Metric.type must be one of counter|histogram|summary.
// - Query.kind must be one of rate|count|quantile.
// - Query.kind must be compatible with Metric.type:
//   - counter -> rate|count
//   - histogram -> quantile
//   - summary -> quantile
//
// - If Query.kind=quantile, Query.quantile must be set.
type StructuredQuery struct {
	// Metric identifies the metric name and type.
	// +kubebuilder:validation:Required
	Metric MetricRef `json:"metric"`

	// Query defines how to compute a value from the metric over a lookback window.
	// +kubebuilder:validation:Required
	Query MetricQuery `json:"query"`

	// Match scopes the query to specific label sets and optional group-by dimensions.
	// +kubebuilder:validation:Required
	Match MetricMatch `json:"match"`
}

// MetricRef identifies a Prometheus metric and its type.
//
// Supported types:
// - counter
// - histogram
// - summary
//
// Admission policy:
//   - metric.type must be one of counter|histogram|summary (Enum).
//   - metric.name must be non-empty (MinLength=1).
//   - If metric.type=histogram, metric.name SHOULD reference the bucket series
//     (typically *_bucket). If not, the controller/webhook should reject to prevent
//     invalid histogram_quantile() queries.
type MetricRef struct {
	// Name is the Prometheus metric name to query.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type declares how the metric should be interpreted.
	// +kubebuilder:validation:Enum=counter;histogram;summary
	Type string `json:"type"`
}

// MetricQuery defines how to compute an observed value.
//
// Supported kinds:
// - rate:     per-second rate over window (for counters)
// - count:    total events in window via increase() (for counters)
// - quantile: latency quantile (for histogram or summary)
//
// Admission policy (cross-field invariants):
// - counter -> rate|count
// - histogram -> quantile
// - summary -> quantile
// - If kind=quantile, quantile must be set (0..1).
type MetricQuery struct {
	// Kind selects the computation method.
	// +kubebuilder:validation:Enum=rate;count;quantile
	Kind string `json:"kind"`

	// Aggregation determines how to aggregate series before comparison.
	// If groupBy is set, aggregation is applied "by (groupBy...)".
	// Defaults to "sum" for counters and "sum" for histogram buckets.
	// +kubebuilder:validation:Enum=sum;avg;max;min
	// +optional
	Aggregation string `json:"aggregation,omitempty"`

	// Quantile is required when kind=quantile.
	// For histograms, this maps to histogram_quantile(q, ...).
	// For summaries, this maps to selecting quantile-labeled series.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	Quantile *float64 `json:"quantile,omitempty"`
}

// MetricMatch scopes a structured query.
//
// Labels are dynamic and organization-specific. Each entry is a Prometheus label matcher:
// - If value starts with "~", it is treated as a regex matcher (k=~"re").
// - Otherwise, it is treated as an exact matcher (k="v").
//
// GroupBy is optional. If set, the query returns a vector with one series per group,
// and ANY-breach semantics apply.
type MetricMatch struct {
	// Labels is a set of dynamic label matchers (exact or regex).
	// At least one label SHOULD be provided to avoid cluster-wide queries.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// GroupBy lists label keys to group results by (e.g., ["pod"]).
	// +optional
	GroupBy []string `json:"groupBy,omitempty"`
}

// MetricCompare defines a threshold comparator for a rule.
//
// Ops:
// - GT, GTE, LT, LTE, EQ, NEQ
//
// Note: EQ/NEQ should be implemented with a small tolerance in the controller
// due to floating point representation.
//
// Admission policy:
// - op must be one of GT|GTE|LT|LTE|EQ|NEQ (Enum).
// - threshold must be a valid number (finite).
type MetricCompare struct {
	// Op defines the comparison operator.
	// +kubebuilder:validation:Enum=GT;GTE;LT;LTE;EQ;NEQ
	Op string `json:"op"`

	// Threshold is the numeric threshold to compare against.
	Threshold float64 `json:"threshold"`
}
