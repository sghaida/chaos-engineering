package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

/*
FaultInjectionSpec defines a single, time-bound chaos experiment executed
via Istio routing primitives (VirtualService patching).

A FaultInjection may contain multiple independent mesh fault actions,
each of which can target either inbound or outbound HTTP traffic.
*/
type FaultInjectionSpec struct {
	// BlastRadius defines temporal and safety constraints for the experiment.
	// The controller MUST enforce these limits.
	// +kubebuilder:validation:Required
	BlastRadius BlastRadiusSpec `json:"blastRadius"`

	// Actions defines the concrete chaos actions to execute.
	// At least one mesh fault must be specified.
	// +kubebuilder:validation:Required
	Actions ActionsSpec `json:"actions"`
}

/*
BlastRadiusSpec defines safety boundaries for a FaultInjection.

These fields exist to prevent accidental wide-impact chaos experiments
and are enforced by the controller and/or admission webhook.
*/
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

/*
ActionsSpec groups all chaos actions associated with a FaultInjection.
*/
type ActionsSpec struct {
	// MeshFaults defines Istio-based HTTP fault injections.
	// Each entry is executed independently.
	// +kubebuilder:validation:MinItems=1
	MeshFaults []MeshFaultAction `json:"meshFaults"`
}

/*
MeshFaultAction defines a single HTTP fault injection action applied
either to inbound or outbound traffic.

Each MeshFaultAction:
- targets exactly one traffic direction (INBOUND or OUTBOUND)
- affects a bounded percentage of traffic
- is independently cleaned up by name
*/
type MeshFaultAction struct {
	// Name uniquely identifies this fault within the FaultInjection.
	//
	// It is used to generate deterministic VirtualService rule names,
	// enabling safe updates and cleanup.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Direction specifies whether the fault applies to inbound
	// traffic (requests entering the service) or outbound traffic
	// (requests leaving the service to a dependency).
	//
	// INBOUND  → patch an existing VirtualService for the service
	// OUTBOUND → patch or create a VirtualService targeting the destination host
	// +kubebuilder:validation:Enum=INBOUND;OUTBOUND
	Direction string `json:"direction"`

	// Type defines the kind of HTTP fault to inject.
	//
	// HTTP_LATENCY → fixed delay injection
	// HTTP_ABORT   → HTTP error response injection
	// +kubebuilder:validation:Enum=HTTP_LATENCY;HTTP_ABORT
	Type string `json:"type"`

	// Percent defines the percentage of matching traffic
	// affected by this fault.
	//
	// This value MUST be <= blastRadius.maxTrafficPercent.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`

	// HTTP defines how the fault is applied at the HTTP routing layer.
	// +kubebuilder:validation:Required
	HTTP HTTPFaultSpec `json:"http"`
}

/*
HTTPFaultSpec defines how a mesh fault is applied to HTTP traffic.

The meaning of fields depends on the fault Direction:

INBOUND:
- virtualServiceRef MUST be set
- destinationHosts MUST NOT be set

OUTBOUND:
- destinationHosts MUST be set
- virtualServiceRef MAY be omitted (operator may create a managed VS)
*/
type HTTPFaultSpec struct {
	// VirtualServiceRef references an existing VirtualService to patch.
	//
	// Required when direction=INBOUND.
	// Optional when direction=OUTBOUND.
	// +optional
	VirtualServiceRef *VirtualServiceRef `json:"virtualServiceRef,omitempty"`

	// DestinationHosts defines the destination hostnames for outbound faults.
	//
	// These are typically external domains (e.g. api.vendor.com) or
	// internal service hostnames.
	//
	// Required when direction=OUTBOUND.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DestinationHosts []string `json:"destinationHosts,omitempty"`

	// SourceSelector restricts OUTBOUND faults to traffic originating
	// from workloads matching these labels.
	//
	// Strongly recommended to avoid impacting all mesh workloads.
	// +optional
	SourceSelector *LabelSelector `json:"sourceSelector,omitempty"`

	// Routes define HTTP match conditions under which the fault applies.
	// These routes are prepended before existing VirtualService rules.
	// +kubebuilder:validation:MinItems=1
	Routes []HTTPRouteTarget `json:"routes"`

	// Delay defines fixed latency injection.
	// Required for HTTP_LATENCY.
	// +optional
	Delay *DelayConfig `json:"delay,omitempty"`

	// Abort defines HTTP abort (error) injection.
	// Required for HTTP_ABORT.
	// +optional
	Abort *AbortConfig `json:"abort,omitempty"`

	// Timeout defines an HTTP route timeout.
	//
	// Note:
	// Istio timeouts are not percentage-based.
	// To simulate percentage-based timeouts, configure:
	//   delay.fixedDelaySeconds > timeout.timeoutSeconds
	// +optional
	Timeout *TimeoutConfig `json:"timeout,omitempty"`
}

/*
VirtualServiceRef identifies an existing Istio VirtualService
in the same namespace.
*/
type VirtualServiceRef struct {
	// Name of the VirtualService.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

/*
LabelSelector defines a simple label-based workload selector.
*/
type LabelSelector struct {
	// MatchLabels restricts matching to workloads
	// that have all of the specified labels.
	// +kubebuilder:validation:MinProperties=1
	MatchLabels map[string]string `json:"matchLabels"`
}

/*
HTTPRouteTarget defines a single HTTP routing match.
*/
type HTTPRouteTarget struct {
	// Match defines how incoming requests are matched.
	// +kubebuilder:validation:Required
	Match HTTPMatch `json:"match"`
}

/*
HTTPMatch defines HTTP request matching criteria.

VALIDATION RULE (enforced by controller or admission webhook):
- At least ONE of uriPrefix or uriExact MUST be specified.
*/
type HTTPMatch struct {
	// URIPrefix matches requests whose URI starts with this prefix.
	// Example: /VendorAPI
	// +kubebuilder:validation:MinLength=1
	// +optional
	URIPrefix string `json:"uriPrefix,omitempty"`

	// URIExact matches requests whose URI exactly equals this value.
	// Example: /VendorAPI/v1/orders
	// +kubebuilder:validation:MinLength=1
	// +optional
	URIExact string `json:"uriExact,omitempty"`

	// Headers defines optional exact-match HTTP headers.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

/*
DelayConfig defines fixed latency injection parameters.

All values are expressed in SECONDS for human safety and clarity.
*/
type DelayConfig struct {
	// FixedDelaySeconds is the artificial latency added to matching requests.
	// Example: 2 → "2s"
	// +kubebuilder:validation:Minimum=1
	FixedDelaySeconds int32 `json:"fixedDelaySeconds"`
}

/*
TimeoutConfig defines HTTP route timeout parameters.

Values are expressed in SECONDS.
*/
type TimeoutConfig struct {
	// TimeoutSeconds is the maximum allowed duration for a request.
	// Example: 1 → "1s"
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

/*
AbortConfig defines HTTP abort (error response) injection.
*/
type AbortConfig struct {
	// HTTPStatus is the HTTP response code returned for aborted requests.
	// Valid range: 100–599
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	HTTPStatus int32 `json:"httpStatus"`
}

/*
FaultInjectionStatus reflects the observed lifecycle state
of a FaultInjection.
*/
type FaultInjectionStatus struct {
	// Phase represents the current lifecycle phase of the experiment.
	//
	// Possible values:
	// - Pending
	// - Running
	// - Completed
	// - Error
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Error
	Phase string `json:"phase,omitempty"`

	// StartedAt is the timestamp when the experiment actually began.
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ExpiresAt is the timestamp when the experiment is scheduled to end.
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Message provides a human-readable status explanation.
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
