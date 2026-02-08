package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FaultInjectionSpec defines a single, time-bound chaos experiment executed
// via Istio routing primitives (VirtualService patching).
//
// A FaultInjection may contain multiple independent mesh fault actions,
// each of which can target either inbound or outbound HTTP traffic.
// The experiment is governed by a BlastRadius that defines safety boundaries
// (max traffic percent, max pods affected, duration).
//
// Optional StopConditions provide automatic kill-switch capabilities
// based on user-defined rules evaluated against Prometheus metrics.
//
// The experiment can be cancelled externally via spec.cancel,
// which triggers immediate cleanup of all injected faults.
type FaultInjectionSpec struct {
	// BlastRadius defines temporal and safety constraints for the experiment.
	// The controller MUST enforce these limits.
	// +kubebuilder:validation:Required
	BlastRadius BlastRadiusSpec `json:"blastRadius"`

	// Actions defines the concrete chaos actions to execute.
	// At least one of meshFaults or podFaults must be specified.
	// +kubebuilder:validation:Required
	Actions ActionsSpec `json:"actions"`

	// Cancel requests immediate stop + cleanup.
	// This is the external stop mechanism for GitOps workflows (e.g., Argo patch).
	// Admission policy: none (boolean toggle).
	// +optional
	Cancel bool `json:"cancel,omitempty"`

	// StopConditions define automatic kill-switch rules.
	//
	// Admission policy (cross-field invariants):
	// - If stopConditions is set, rules MUST be non-empty (enforced by MinItems=1 on Rules).
	// - If stopConditions.rules has at least one entry, failOnNoMetrics MUST default to true
	//   unless explicitly set to false by the user (defaulting webhook).
	// - Exactly one of rule.promql or rule.structured MUST be set for every rule.
	// - If any rule omits intervalSeconds or windowSeconds, defaults MUST be provided and must
	//   supply the missing values.
	// - If overallBreachesCount is set, consecutiveBreaches MUST NOT be present anywhere
	//   (neither defaults.consecutiveBreaches nor any rule.consecutiveBreaches).
	// - If overallBreachesCount is NOT set, consecutiveBreaches MUST be defined either
	//   in defaults OR on ALL rules; otherwise admission MUST reject.
	// +optional
	StopConditions *StopConditionsSpec `json:"stopConditions,omitempty"`
}

// BlastRadiusSpec defines safety boundaries for a FaultInjection.
//
// These fields exist to prevent accidental wide-impact chaos experiments
// and are enforced by the controller and/or admission webhook.
// Examples:
// - maxTrafficPercent: 10%
// - maxPodsAffected:  5
// - durationSeconds:  300 (5 minutes)
// - scope:            namespace
// Note: scope is informational for now and reserved for future policy enforcement.
type BlastRadiusSpec struct {
	// DurationSeconds is the total runtime of the experiment.
	// After this duration expires, all injected faults are automatically cleaned up.
	//
	// Example: 300 (5 minutes)
	// +kubebuilder:validation:Minimum=1
	DurationSeconds int64 `json:"durationSeconds"`

	// MaxTrafficPercent is an upper bound on how much traffic
	// any mesh fault is allowed to affect.
	//
	// Each meshFault.percent MUST be <= this value.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	MaxTrafficPercent int32 `json:"maxTrafficPercent"`

	// Scope is informational for now and reserved for future policy enforcement.
	// Currently only "namespace" is supported.
	// +kubebuilder:validation:Enum=namespace
	// +optional
	Scope string `json:"scope,omitempty"`

	// MaxPodsAffected is a safety guardrail limiting the number of pods
	// that may be impacted by the experiment.
	//
	// This is enforced using http.sourceSelector.matchLabels on actions.
	// If set and the controller cannot safely estimate affected pods,
	// the experiment will be aborted.
	//
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxPodsAffected int64 `json:"maxPodsAffected,omitempty"`
}

// ActionsSpec groups all chaos actions associated with a FaultInjection.
// Each action is executed independently.
// +kubebuilder:validation:XValidation:rule="(has(self.meshFaults) && size(self.meshFaults) > 0) || (has(self.podFaults) && size(self.podFaults) > 0)",message="at least one of actions.meshFaults or actions.podFaults must be specified"
type ActionsSpec struct {
	// MeshFaults defines Istio-based HTTP fault injections.
	// Each entry is executed independently.
	// +optional
	MeshFaults []MeshFaultAction `json:"meshFaults,omitempty"`

	// PodFaults defines pod-level faults (bounded and time-boxed).
	// Each entry is executed independently.
	// +optional
	PodFaults []PodFaultAction `json:"podFaults,omitempty"`
}

// StopConditionsSpec defines automatic kill-switch criteria for a FaultInjection experiment.
//
// The controller evaluates a list of rules periodically while the experiment is Running.
// Rules may be expressed in one of two ways:
//
//   - Structured rules: metric + type + query method + dynamic label matchers + optional groupBy
//   - Raw PromQL rules: an escape hatch for advanced Prometheus queries
//
// Mixing both structured and raw PromQL rules in the same experiment is supported.
//
// Breach accounting modes:
//
//   - Overall mode: when overallBreachesCount is set, per-rule consecutiveBreaches MUST NOT
//     be present anywhere (neither defaults.consecutiveBreaches nor any rule.consecutiveBreaches).
//     In this mode, ANY rule breach increments a single overall breach counter. When the counter
//     reaches overallBreachesCount, the experiment is stopped.
//
//   - Per-rule mode: when overallBreachesCount is NOT set, consecutiveBreaches MUST be defined
//     either in defaults OR on ALL rules. If any rule cannot resolve an effective
//     consecutiveBreaches, admission MUST reject.
//
// Defaults and validation:
//
//   - If any rule omits intervalSeconds or windowSeconds, defaults MUST be provided
//     and MUST supply the missing values.
//
// Metrics failure semantics:
//
//   - If stopConditions.rules has at least one entry, failOnNoMetrics MUST default to true
//     unless explicitly set to false by the user.
//   - When failOnNoMetrics is true, missing metrics ("no data") and evaluation errors
//     (e.g., malformed query, Prometheus unavailable) are treated as breaches and can trigger
//     the kill switch.
//
// Admission policy (cross-field invariants):
// - rules must have at least one entry (MinItems=1).
// - If overallBreachesCount is set:
//   - defaults.consecutiveBreaches MUST be omitted/0.
//   - rule.consecutiveBreaches MUST be omitted/0 for all rules.
//
// - If overallBreachesCount is NOT set:
//   - Either defaults.consecutiveBreaches > 0, OR every rule must set rule.consecutiveBreaches > 0.
//
// - If any rule omits intervalSeconds or windowSeconds, defaults must be present and cover them.
// - failOnNoMetrics should be defaulted to true when rules is non-empty (defaulting webhook).
type StopConditionsSpec struct {
	// OverallBreachesCount enables "overall" stop mode.
	//
	// Admission policy:
	// - If set, consecutiveBreaches MUST NOT be present anywhere (defaults or rules).
	// +kubebuilder:validation:Minimum=1
	// +optional
	OverallBreachesCount *int32 `json:"overallBreachesCount,omitempty"`

	// FailOnNoMetrics controls whether missing/invalid metrics should trigger the kill switch.
	//
	// Defaulting policy:
	// - If rules has at least one entry, this MUST default to true unless explicitly set.
	//   (Implement via a defaulter webhook; pointer form allows distinguishing unset vs false.)
	// +optional
	FailOnNoMetrics *bool `json:"failOnNoMetrics,omitempty"`

	// Defaults defines optional per-experiment defaults for rule evaluation.
	//
	// Admission policy:
	// - Required if any rule omits intervalSeconds or windowSeconds.
	// - In per-rule mode (overallBreachesCount unset), required unless every rule specifies
	//   consecutiveBreaches explicitly.
	// - In overall mode (overallBreachesCount set), defaults.consecutiveBreaches MUST NOT be set.
	// +optional
	Defaults *StopDefaults `json:"defaults,omitempty"`

	// Rules is the set of kill-switch rules.
	// At least one rule must be provided when stopConditions is set.
	// +kubebuilder:validation:MinItems=1
	Rules []StopRule `json:"rules"`
}

// StopDefaults provides default evaluation parameters applied to rules that do not
// set their own overrides.
//
// Effective values:
// - intervalSeconds: rule.intervalSeconds if set, else defaults.intervalSeconds
// - windowSeconds:   rule.windowSeconds if set, else defaults.windowSeconds
// - consecutiveBreaches (per-rule mode only): rule.consecutiveBreaches if set, else defaults.consecutiveBreaches
//
// Admission policy (cross-field invariants):
//   - If any rule omits intervalSeconds or windowSeconds, Defaults must exist and supply them.
//   - In per-rule mode (overallBreachesCount unset), Defaults.consecutiveBreaches must be set
//     unless every rule defines rule.consecutiveBreaches.
//   - In overall mode (overallBreachesCount set), Defaults.consecutiveBreaches MUST NOT be set.
type StopDefaults struct {
	// IntervalSeconds is how often rules are evaluated by default.
	// +kubebuilder:validation:Minimum=1
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// WindowSeconds is the lookback window for rate/count computations.
	// +kubebuilder:validation:Minimum=10
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// ConsecutiveBreaches is the default debounce count before triggering a stop.
	//
	// Admission policy:
	// - In per-rule mode (overallBreachesCount unset), required unless all rules set their own
	//   consecutiveBreaches explicitly.
	// - In overall mode (overallBreachesCount set), MUST NOT be present.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ConsecutiveBreaches int32 `json:"consecutiveBreaches,omitempty"`
}

// StopRule defines one kill-switch rule.
//
// Rules can be expressed either as:
// - promql: raw PromQL string (escape hatch), OR
// - structured: a structured query definition (metric/type/method/labels)
//
// Exactly ONE of promql or structured MUST be set (enforced by controller or webhook).
//
// Evaluation semantics:
//   - The rule is evaluated only when it becomes "due" (based on intervalSeconds).
//   - If the query yields multiple time series (e.g. groupBy=["pod"]), the controller
//     treats the rule as breached if ANY series breaches the threshold (safer default).
//
// Admission policy (cross-field invariants):
//   - Exactly one of promql or structured must be set.
//   - If overallBreachesCount is set, consecutiveBreaches MUST NOT be set on any rule.
//   - If overallBreachesCount is NOT set, consecutiveBreaches MUST be defined either
//     in defaults OR on ALL rules.
type StopRule struct {
	// Name is a stable identifier for this rule, used for status reporting.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// IntervalSeconds overrides the default interval for this rule.
	//
	// Admission policy:
	// - If omitted, defaults.intervalSeconds must be set.
	// +kubebuilder:validation:Minimum=1
	// +optional
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// WindowSeconds overrides the default lookback window for this rule.
	//
	// Admission policy:
	// - If omitted, defaults.windowSeconds must be set.
	// +kubebuilder:validation:Minimum=10
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// ConsecutiveBreaches overrides the default debounce count for this rule.
	//
	// Admission policy:
	// - If overallBreachesCount is set, MUST NOT be present.
	// - If overallBreachesCount is NOT set, must be resolvable either by setting this field
	//   on ALL rules or by setting defaults.consecutiveBreaches.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ConsecutiveBreaches int32 `json:"consecutiveBreaches,omitempty"`

	// PromQL is a raw Prometheus query to evaluate (escape hatch).
	//
	// Admission policy:
	// - Exactly one of promql or structured must be set.
	// +optional
	PromQL string `json:"promql,omitempty"`

	// Structured defines a safe, label-agnostic query builder.
	//
	// Admission policy:
	// - Exactly one of promql or structured must be set.
	// +optional
	Structured *StructuredQuery `json:"structured,omitempty"`

	// Compare defines how the observed value is compared against the threshold.
	// +kubebuilder:validation:Required
	Compare MetricCompare `json:"compare"`
}

// LabelSelector defines a simple label-based workload selector.
// Used to scope outbound faults to specific source workloads.
type LabelSelector struct {
	// MatchLabels restricts matching to workloads that have all of the specified labels.
	// +kubebuilder:validation:MinProperties=1
	MatchLabels map[string]string `json:"matchLabels"`
}

// DelayConfig defines fixed latency injection parameters.
// All values are expressed in SECONDS for human safety and clarity.
type DelayConfig struct {
	// FixedDelaySeconds is the artificial latency added to matching requests.
	// Example: 2 → "2s"
	// +kubebuilder:validation:Minimum=1
	FixedDelaySeconds int32 `json:"fixedDelaySeconds"`
}

// TimeoutConfig defines HTTP route timeout parameters.
// Values are expressed in SECONDS.
// Note: Istio timeouts are not percentage-based.
// To simulate percentage-based timeouts, configure:
// delay.fixedDelaySeconds > timeout.timeoutSeconds
type TimeoutConfig struct {
	// TimeoutSeconds is the maximum allowed duration for a request.
	// Example: 1 → "1s"
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// AbortConfig defines HTTP abort (error response) injection.
// All values are expressed as standard HTTP status codes.
type AbortConfig struct {
	// HTTPStatus is the HTTP response code returned for aborted requests.
	// Valid range: 100–599
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	HTTPStatus int32 `json:"httpStatus"`
}

// FaultInjectionStatus reflects the observed lifecycle state of a FaultInjection.
type FaultInjectionStatus struct {
	// Phase represents the current lifecycle phase of the experiment.
	//
	// Possible values:
	// - Pending
	// - Running
	// - Completed
	// - Error
	// - Cancelled
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Error;Cancelled
	Phase string `json:"phase,omitempty"`

	// StartedAt is the timestamp when the experiment actually began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ExpiresAt is the timestamp when the experiment is scheduled to end.
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Message provides a human-readable status explanation.
	Message string `json:"message,omitempty"`

	// CancelledAt is set when the experiment is stopped externally (spec.cancel)
	// or automatically (stopConditions).
	// +optional
	CancelledAt *metav1.Time `json:"cancelledAt,omitempty"`

	// StopReason indicates why the experiment was stopped (external or which rule).
	// +optional
	StopReason string `json:"stopReason,omitempty"`

	// Rules captures per-rule evaluation state (interval scheduling + debouncing).
	// +optional
	Rules []StopRuleStatus `json:"rules,omitempty"`

	// PodFaults records per-action pod fault execution state.
	// +optional
	PodFaults []PodFaultTickStatus `json:"podFaults,omitempty"`
}

// StopRuleStatus stores per-rule evaluation state.
//
// This allows the controller to:
// - evaluate only "due" rules (per-rule dynamic intervals)
// - report the last observed value for debugging
// - report the last PromQL used (built or raw) for debugging
// - report the last evaluation outcome (breached/ok/error/no-data)
//
// Note: breach counters are still useful for debugging even if you run in overall mode.
type StopRuleStatus struct {
	// Name matches StopRule.name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// LastEvaluatedAt records when this rule was last evaluated.
	// +optional
	LastEvaluatedAt *metav1.Time `json:"lastEvaluatedAt,omitempty"`

	// BreachCount tracks consecutive breaches for this rule (per-rule mode),
	// or how many times this rule breached (overall mode) for debugging.
	// +optional
	BreachCount int32 `json:"breachCount,omitempty"`

	// LastObserved is the last observed value for this rule (scalar or max over vector).
	// +optional
	LastObserved float64 `json:"lastObserved,omitempty"`

	// LastQuery is the last PromQL used (built or raw) for this rule, for debugging.
	// +optional
	LastQuery string `json:"lastQuery,omitempty"`

	// Message contains the last evaluation outcome (breached/ok/error/no-data).
	// +optional
	Message string `json:"message,omitempty"`
}

// FaultInjection is the Schema for the faultinjections API
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type FaultInjection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state of the FaultInjection.
	// +kubebuilder:validation:Required
	Spec FaultInjectionSpec `json:"spec"`

	// Status reflects the observed state of the FaultInjection.
	Status FaultInjectionStatus `json:"status,omitempty"`
}

// FaultInjectionList contains a list of FaultInjection
// +kubebuilder:object:root=true
type FaultInjectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FaultInjection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FaultInjection{}, &FaultInjectionList{})
}
